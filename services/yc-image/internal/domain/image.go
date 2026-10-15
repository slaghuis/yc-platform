// Package domain holds yc-image's model.
package domain

import (
	"regexp"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is an image's lifecycle state.
type Status string

// Statuses.
const (
	StatusPending    Status = "pending" // presigned upload not finalised
	StatusProcessing Status = "processing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusDeleted    Status = "deleted"
)

var ownerRefPattern = regexp.MustCompile(`^[a-z]+:[0-9a-f-]{36}$`)

// ValidateOwnerRef checks an optional owner reference such as
// "product:<uuid>".
func ValidateOwnerRef(ref string) error {
	if ref != "" && !ownerRefPattern.MatchString(ref) {
		return ycerrors.Validation().Field("owner_ref", "invalid", "Must be <kind>:<uuid>, e.g. product:…").Build()
	}
	return nil
}

// Image is an uploaded image and, once ready, its variants.
type Image struct {
	ID            string
	TenantID      string
	UploaderID    string
	Status        Status
	ContentType   string
	Width         int
	Height        int
	Bytes         int64
	Checksum      string
	OwnerRef      string
	FailureReason string
	CreatedAt     time.Time
	ProcessedAt   *time.Time
	Variants      []Variant
}

// Variant is one stored rendition.
type Variant struct {
	Variant string
	Format  string
	Width   int
	Height  int
	Bytes   int64
	Key     string
	URL     string
}

// Find returns the variant with the given name and format.
func (img Image) Find(variant, format string) (Variant, bool) {
	for _, v := range img.Variants {
		if v.Variant == variant && v.Format == format {
			return v, true
		}
	}
	return Variant{}, false
}

// Original returns the re-encoded upload, which has a single format.
func (img Image) Original() (Variant, bool) {
	for _, v := range img.Variants {
		if v.Variant == "original" {
			return v, true
		}
	}
	return Variant{}, false
}

// StagingKey is where an upload waits for processing.
func StagingKey(tenantID, imageID string) string {
	return Prefix(tenantID, imageID) + "upload"
}

// Prefix holds every object of an image.
func Prefix(tenantID, imageID string) string {
	return tenantID + "/" + imageID + "/"
}
