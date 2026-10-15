// Package service holds yc-image's use cases. Uploads are validated and
// stored, then processed asynchronously by yc-image's own consumer of
// image.uploaded.v1.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/imaging"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/repository"
)

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Store is the object storage the service writes to.
type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string, maxBytes int64) ([]byte, error)
	DeletePrefix(ctx context.Context, prefix string) error
	PresignPut(ctx context.Context, key, contentType string, now time.Time) (string, time.Time, error)
	URL(key string) string
}

// Service implements the image use cases.
type Service struct {
	db      *pgxpool.Pool
	bus     Publisher
	store   Store
	repo    repository.Images
	limits  imaging.Limits
	quality imaging.Quality
	now     func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, store Store, limits imaging.Limits, quality imaging.Quality) *Service {
	return &Service{db: db, bus: bus, store: store, limits: limits, quality: quality, now: time.Now}
}

// UploadURL is where a client PUTs a presigned upload.
type UploadURL struct {
	URL       string
	ExpiresAt time.Time
}

// Upload validates and stores a direct upload, then queues it for
// processing.
func (s *Service) Upload(ctx context.Context, data []byte, ownerRef string) (domain.Image, error) {
	a, err := shopAdmin(ctx)
	if err != nil {
		return domain.Image{}, err
	}
	if err := domain.ValidateOwnerRef(ownerRef); err != nil {
		return domain.Image{}, err
	}
	info, err := imaging.Inspect(data, s.limits)
	if err != nil {
		return domain.Image{}, rejected("file", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Image{}, err
	}
	img := domain.Image{
		ID: id.String(), TenantID: a.TenantID, UploaderID: a.ActorID, Status: domain.StatusProcessing,
		ContentType: info.ContentType, Bytes: int64(len(data)), Checksum: checksum(data), OwnerRef: ownerRef,
		CreatedAt: s.now().UTC(),
	}
	key := domain.StagingKey(img.TenantID, img.ID)
	if err := s.store.Put(ctx, key, data, info.ContentType); err != nil {
		return domain.Image{}, ycerrors.Unavailable("Image storage is unavailable").Wrap(err)
	}
	err = dbkit.WithTenantTx(ctx, s.db, img.TenantID, func(tx pgx.Tx) error {
		if err := s.repo.Create(ctx, tx, img); err != nil {
			return err
		}
		return s.publishUploaded(ctx, tx, img)
	})
	if err != nil {
		s.cleanup(ctx, img.TenantID, img.ID)
		return domain.Image{}, err
	}
	return img, nil
}

// RequestUpload creates a pending image and a presigned URL to upload it
// to directly.
func (s *Service) RequestUpload(ctx context.Context, contentType string, size int64, ownerRef string) (domain.Image, UploadURL, error) {
	a, err := shopAdmin(ctx)
	if err != nil {
		return domain.Image{}, UploadURL{}, err
	}
	v := ycerrors.Validation()
	if !imaging.AllowedType(contentType) {
		v.Field("content_type", imaging.ReasonUnsupportedFormat, "Must be image/jpeg, image/png, image/webp or image/gif")
	}
	if size < 1 || size > s.limits.MaxBytes {
		v.Field("bytes", imaging.ReasonTooLarge, "Must be between 1 and the upload limit")
	}
	if v.HasErrors() {
		return domain.Image{}, UploadURL{}, v.Build()
	}
	if err := domain.ValidateOwnerRef(ownerRef); err != nil {
		return domain.Image{}, UploadURL{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Image{}, UploadURL{}, err
	}
	now := s.now().UTC()
	img := domain.Image{
		ID: id.String(), TenantID: a.TenantID, UploaderID: a.ActorID, Status: domain.StatusPending,
		ContentType: contentType, Bytes: size, OwnerRef: ownerRef, CreatedAt: now,
	}
	var up UploadURL
	if up.URL, up.ExpiresAt, err = s.store.PresignPut(ctx, domain.StagingKey(img.TenantID, img.ID), contentType, now); err != nil {
		return domain.Image{}, UploadURL{}, err
	}
	err = dbkit.WithTenantTx(ctx, s.db, img.TenantID, func(tx pgx.Tx) error { return s.repo.Create(ctx, tx, img) })
	return img, up, err
}

// Finalize validates a presigned upload and queues it for processing. An
// invalid upload marks the image failed.
func (s *Service) Finalize(ctx context.Context, id string) (domain.Image, error) {
	a, err := shopAdmin(ctx)
	if err != nil {
		return domain.Image{}, err
	}
	img, err := s.pending(ctx, a.TenantID, id)
	if err != nil {
		return domain.Image{}, err
	}
	data, err := s.store.Get(ctx, domain.StagingKey(img.TenantID, img.ID), s.limits.MaxBytes)
	switch {
	case errors.Is(err, integration.ErrNotFound):
		return domain.Image{}, ycerrors.Conflict("Nothing has been uploaded for this image")
	case errors.Is(err, integration.ErrTooLarge):
		err = &imaging.Failure{Reason: imaging.ReasonTooLarge, Detail: "upload exceeds the limit"}
	case err != nil:
		return domain.Image{}, ycerrors.Unavailable("Image storage is unavailable").Wrap(err)
	default:
		var info imaging.Info
		if info, err = imaging.Inspect(data, s.limits); err == nil {
			img.ContentType, img.Bytes, img.Checksum = info.ContentType, int64(len(data)), checksum(data)
		}
	}
	if f, ok := imaging.AsFailure(err); ok {
		if ferr := s.fail(ctx, img, f); ferr != nil {
			return domain.Image{}, ferr
		}
		s.cleanup(ctx, img.TenantID, img.ID)
		return domain.Image{}, rejected("file", err)
	}

	err = dbkit.WithTenantTx(ctx, s.db, img.TenantID, func(tx pgx.Tx) error {
		cur, err := s.repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.Status != domain.StatusPending {
			return ycerrors.Conflict("The image is not awaiting upload")
		}
		if err := s.repo.MarkProcessing(ctx, tx, id, img.ContentType, img.Bytes, img.Checksum); err != nil {
			return err
		}
		return s.publishUploaded(ctx, tx, img)
	})
	img.Status = domain.StatusProcessing
	return img, err
}

func (s *Service) pending(ctx context.Context, tenantID, id string) (domain.Image, error) {
	var img domain.Image
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		img, err = s.repo.Get(ctx, tx, id, false)
		return err
	})
	if err == nil && img.Status != domain.StatusPending {
		err = ycerrors.Conflict("The image is not awaiting upload")
	}
	return img, err
}

// Process renders a processing image's variants and makes it ready. It is
// idempotent: images in any other state are left alone.
func (s *Service) Process(ctx context.Context, tenantID, id string) error {
	var img domain.Image
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		img, err = s.repo.Get(ctx, tx, id, false)
		return err
	})
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return nil
	}
	if err != nil || img.Status != domain.StatusProcessing {
		return err
	}

	data, err := s.store.Get(ctx, domain.StagingKey(tenantID, id), s.limits.MaxBytes)
	if errors.Is(err, integration.ErrNotFound) {
		return s.fail(ctx, img, &imaging.Failure{Reason: events.ImageProcessingFailedV1ReasonStorageError, Detail: "upload missing"})
	}
	if err != nil {
		return err
	}
	outs, err := imaging.Render(data, s.quality)
	if f, ok := imaging.AsFailure(err); ok {
		return s.fail(ctx, img, f)
	}
	if err != nil {
		return err
	}

	variants := make([]domain.Variant, len(outs))
	for i, o := range outs {
		key := domain.Prefix(tenantID, id) + o.Variant + "." + o.Ext()
		if err := s.store.Put(ctx, key, o.Data, o.ContentType); err != nil {
			return err
		}
		variants[i] = domain.Variant{
			Variant: o.Variant, Format: o.Format, Width: o.Width, Height: o.Height,
			Bytes: int64(len(o.Data)), Key: key, URL: s.store.URL(key),
		}
	}
	orig := outs[0]
	now := s.now().UTC()
	err = dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		cur, err := s.repo.Get(ctx, tx, id, true)
		if err != nil || cur.Status != domain.StatusProcessing {
			return err
		}
		if err := s.repo.MarkReady(ctx, tx, img, orig.Width, orig.Height, variants, now); err != nil {
			return err
		}
		evt := events.ImageProcessedV1{
			ImageID: id, OwnerRef: optional(img.OwnerRef), Width: int64(orig.Width), Height: int64(orig.Height),
			Variants: make([]events.ImageProcessedV1VariantsItem, len(variants)), ProcessedAt: now,
		}
		for i, v := range variants {
			evt.Variants[i] = events.ImageProcessedV1VariantsItem{
				Variant: v.Variant, Format: v.Format, Width: int64(v.Width), Height: int64(v.Height), Bytes: v.Bytes, URL: v.URL,
			}
		}
		return s.publish(ctx, tx, tenantID, events.TypeImageProcessedV1, evt)
	})
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		s.cleanup(ctx, tenantID, id)
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.store.DeletePrefix(ctx, domain.StagingKey(tenantID, id)); err != nil {
		telemetry.LoggerFromContext(ctx).Warn("delete staged upload", "image_id", id, "err", err)
	}
	return nil
}

// fail marks an image failed and reports why.
func (s *Service) fail(ctx context.Context, img domain.Image, f *imaging.Failure) error {
	return dbkit.WithTenantTx(ctx, s.db, img.TenantID, func(tx pgx.Tx) error {
		if err := s.repo.MarkFailed(ctx, tx, img.ID, f.Reason); err != nil {
			return err
		}
		return s.publish(ctx, tx, img.TenantID, events.TypeImageProcessingFailedV1, events.ImageProcessingFailedV1{
			ImageID: img.ID, OwnerRef: optional(img.OwnerRef), Reason: f.Reason, Detail: optional(f.Detail),
			FailedAt: s.now().UTC(),
		})
	})
}

// Get returns an image. Only admins see images that are not ready.
func (s *Service) Get(ctx context.Context, id string) (domain.Image, error) {
	a, _ := authctx.FromContext(ctx)
	var img domain.Image
	read := func(tx pgx.Tx) error {
		var err error
		img, err = s.repo.Get(ctx, tx, id, false)
		return err
	}
	var err error
	switch a.TenantID {
	case "":
		return domain.Image{}, ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case dbkit.PlatformTenantID:
		err = dbkit.WithPlatformTx(ctx, s.db, read)
	default:
		err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, read)
	}
	if err != nil {
		return domain.Image{}, err
	}
	admin := a.ActorType == authctx.ActorShopAdmin || a.ActorType == authctx.ActorSystemAdmin
	if img.Status != domain.StatusReady && !admin {
		return domain.Image{}, ycerrors.NotFound("Image", id)
	}
	return img, nil
}

// VariantURL returns where a ready image's variant is served.
func (s *Service) VariantURL(ctx context.Context, id, variant, format string) (string, error) {
	img, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if img.Status != domain.StatusReady {
		return "", ycerrors.Conflict("The image is not ready")
	}
	if format == "" {
		format = imaging.FormatWebP
	}
	v, ok := img.Find(variant, format)
	if variant == imaging.Original {
		v, ok = img.Original()
	}
	if !ok {
		return "", ycerrors.NotFound("Image variant", variant+"."+format)
	}
	return v.URL, nil
}

// Delete deletes an image and its files.
func (s *Service) Delete(ctx context.Context, id string) error {
	a, err := shopAdmin(ctx)
	if err != nil {
		return err
	}
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		img, err := s.repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if err := s.repo.MarkDeleted(ctx, tx, id, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, a.TenantID, events.TypeImageDeletedV1, events.ImageDeletedV1{
			ImageID: id, OwnerRef: optional(img.OwnerRef), DeletedAt: now,
		})
	})
	if err != nil {
		return err
	}
	s.cleanup(ctx, a.TenantID, id)
	return nil
}

// cleanup deletes an image's files; leftovers are harmless, so failure is
// only logged.
func (s *Service) cleanup(ctx context.Context, tenantID, id string) {
	if err := s.store.DeletePrefix(context.WithoutCancel(ctx), domain.Prefix(tenantID, id)); err != nil {
		telemetry.LoggerFromContext(ctx).Warn("delete image files", "image_id", id, "err", err)
	}
}

func (s *Service) publishUploaded(ctx context.Context, tx pgx.Tx, img domain.Image) error {
	return s.publish(ctx, tx, img.TenantID, events.TypeImageUploadedV1, events.ImageUploadedV1{
		ImageID: img.ID, OwnerRef: optional(img.OwnerRef), ContentType: img.ContentType,
		Bytes: img.Bytes, ChecksumSha256: img.Checksum, UploadedAt: s.now().UTC(),
	})
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	a, _ := authctx.FromContext(ctx)
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: eventType,
		TenantID:  tenantID,
		Actor:     eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI},
		Payload:   payload,
	})
}

// shopAdmin returns the caller, who must be a shop admin: images belong to
// a shop.
func shopAdmin(ctx context.Context) (authctx.AuthContext, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return a, ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := eventbus.ValidateTenantID(a.TenantID); err != nil || a.TenantID == eventbus.PlatformTenantID {
		return a, ycerrors.Validation().Field("tenant_id", "invalid", "Images belong to a shop").Build()
	}
	return a, nil
}

// rejected turns an imaging failure into a validation error on field.
func rejected(field string, err error) error {
	if f, ok := imaging.AsFailure(err); ok {
		return ycerrors.Validation().Field(field, f.Reason, f.Detail).Wrap(err)
	}
	return err
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
