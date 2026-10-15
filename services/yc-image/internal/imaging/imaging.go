// Package imaging validates uploads and renders an image's variants
// (docs/services/yc-image.md, Image Variants). It does no I/O beyond the
// byte slices it is given.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registers the GIF decoder
	"image/jpeg"
	"image/png"
	"net/http"

	"github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"
)

// Content types accepted for upload.
const (
	TypeJPEG = "image/jpeg"
	TypePNG  = "image/png"
	TypeWebP = "image/webp"
	TypeGIF  = "image/gif"
)

// Failure reasons, as in image.processing_failed.v1.
const (
	ReasonUnsupportedFormat = "unsupported_format"
	ReasonCorrupt           = "corrupt"
	ReasonTooLarge          = "too_large"
	ReasonBadDimensions     = "bad_dimensions"
)

// Failure is an upload that cannot become an image.
type Failure struct {
	Reason string
	Detail string
}

func (f *Failure) Error() string { return f.Reason + ": " + f.Detail }

// AsFailure returns err's Failure, if it is one.
func AsFailure(err error) (*Failure, bool) {
	var f *Failure
	ok := errors.As(err, &f)
	return f, ok
}

// Limits bound what is accepted.
type Limits struct {
	MaxBytes       int64   `koanf:"max_upload_bytes"`
	MaxDimension   int     `koanf:"max_dimension"`
	MaxPixels      int     `koanf:"max_pixels"`
	MaxAspectRatio float64 `koanf:"max_aspect_ratio"`
}

// AllowedType reports whether contentType may be uploaded.
func AllowedType(contentType string) bool {
	switch contentType {
	case TypeJPEG, TypePNG, TypeWebP, TypeGIF:
		return true
	}
	return false
}

// Info describes an accepted upload.
type Info struct {
	ContentType string
	Width       int
	Height      int
}

// Inspect checks an upload's size, type and dimensions from its header,
// without decoding the pixels.
func Inspect(data []byte, l Limits) (Info, error) {
	if int64(len(data)) > l.MaxBytes {
		return Info{}, &Failure{ReasonTooLarge, fmt.Sprintf("%d bytes exceeds %d", len(data), l.MaxBytes)}
	}
	if len(data) == 0 {
		return Info{}, &Failure{ReasonCorrupt, "empty upload"}
	}
	ct := http.DetectContentType(data)
	if !AllowedType(ct) {
		return Info{}, &Failure{ReasonUnsupportedFormat, ct + " is not JPEG, PNG, WebP or GIF"}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, &Failure{ReasonCorrupt, err.Error()}
	}
	w, h := cfg.Width, cfg.Height
	switch {
	case w < 1 || h < 1:
		return Info{}, &Failure{ReasonBadDimensions, "empty image"}
	case w > l.MaxDimension || h > l.MaxDimension:
		return Info{}, &Failure{ReasonBadDimensions, fmt.Sprintf("%dx%d exceeds %d", w, h, l.MaxDimension)}
	case w*h > l.MaxPixels:
		return Info{}, &Failure{ReasonBadDimensions, fmt.Sprintf("%dx%d exceeds %d pixels", w, h, l.MaxPixels)}
	case float64(max(w, h))/float64(min(w, h)) > l.MaxAspectRatio:
		return Info{}, &Failure{ReasonBadDimensions, fmt.Sprintf("%dx%d is more elongated than %g:1", w, h, l.MaxAspectRatio)}
	}
	return Info{ContentType: ct, Width: w, Height: h}, nil
}

// Spec is a variant's bounding box: the image is scaled down, keeping its
// aspect ratio, to fit Max×Max. Smaller images are not enlarged.
type Spec struct {
	Name string
	Max  int
}

// Variants is the fixed v1 set, largest first.
var Variants = []Spec{
	{Name: "xlarge", Max: 2048},
	{Name: "large", Max: 1024},
	{Name: "medium", Max: 512},
	{Name: "thumbnail", Max: 200},
}

// Original is the re-encoded upload.
const Original = "original"

// Formats of rendered variants.
const (
	FormatJPEG = "jpeg"
	FormatPNG  = "png"
	FormatWebP = "webp"
)

// Quality configures the encoders.
type Quality struct {
	JPEG int `koanf:"jpeg_quality"`
	WebP int `koanf:"webp_quality"`
}

// Output is one encoded file.
type Output struct {
	Variant     string
	Format      string
	ContentType string
	Width       int
	Height      int
	Data        []byte
}

// Ext returns the file extension for the output's format.
func (o Output) Ext() string {
	if o.Format == FormatJPEG {
		return "jpg"
	}
	return o.Format
}

// Render decodes an inspected upload and encodes the original (re-encoded
// in its own format, which strips EXIF and other metadata; GIF becomes PNG
// of its first frame) plus every variant in WebP and JPEG.
func Render(data []byte, q Quality) ([]Output, error) {
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, &Failure{ReasonCorrupt, err.Error()}
	}
	b := src.Bounds()
	out := make([]Output, 0, 1+2*len(Variants))

	var orig Output
	switch format {
	case "jpeg":
		orig, err = encode(src, FormatJPEG, 95)
	case "webp":
		orig, err = encode(src, FormatWebP, 95)
	default: // png, and gif's first frame
		orig, err = encode(src, FormatPNG, 0)
	}
	if err != nil {
		return nil, err
	}
	orig.Variant = Original
	out = append(out, orig)

	// Each variant is scaled from the previous, larger one: much faster
	// than scaling every size from a large original, at no visible cost.
	prev := src
	for _, spec := range Variants {
		w, h := fit(b.Dx(), b.Dy(), spec.Max)
		img := prev
		if pb := prev.Bounds(); pb.Dx() != w || pb.Dy() != h {
			dst := image.NewNRGBA(image.Rect(0, 0, w, h))
			xdraw.CatmullRom.Scale(dst, dst.Bounds(), prev, pb, draw.Src, nil)
			img = dst
		}
		prev = img
		for _, f := range []struct {
			format  string
			quality int
		}{{FormatWebP, q.WebP}, {FormatJPEG, q.JPEG}} {
			o, err := encode(img, f.format, f.quality)
			if err != nil {
				return nil, err
			}
			o.Variant = spec.Name
			out = append(out, o)
		}
	}
	return out, nil
}

// fit scales w×h down to fit within limit×limit.
func fit(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	if w >= h {
		return limit, max(1, (h*limit+w/2)/w)
	}
	return max(1, (w*limit+h/2)/h), limit
}

func encode(img image.Image, format string, quality int) (Output, error) {
	var buf bytes.Buffer
	var err error
	ct := ""
	switch format {
	case FormatJPEG:
		ct = TypeJPEG
		err = jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: quality})
	case FormatWebP:
		ct = TypeWebP
		err = webp.Encode(&buf, img, webp.Options{Quality: quality})
	case FormatPNG:
		ct = TypePNG
		err = png.Encode(&buf, img)
	default:
		err = fmt.Errorf("imaging: unknown format %q", format)
	}
	if err != nil {
		return Output{}, fmt.Errorf("encode %s: %w", format, err)
	}
	b := img.Bounds()
	return Output{Format: format, ContentType: ct, Width: b.Dx(), Height: b.Dy(), Data: buf.Bytes()}, nil
}

// flatten composites transparent images onto white, as JPEG has no alpha.
func flatten(img image.Image) image.Image {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return img
	}
	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Over)
	return dst
}
