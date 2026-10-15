package imaging_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/gen2brain/webp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-image/internal/imaging"
)

var limits = imaging.Limits{MaxBytes: 1 << 20, MaxDimension: 4000, MaxPixels: 8_000_000, MaxAspectRatio: 10}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: uint8(128 + x%128)})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// jpegWithExif returns a JPEG carrying an APP1 Exif segment that contains
// marker.
func jpegWithExif(t *testing.T, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 64, 48)), nil))
	payload := append([]byte("Exif\x00\x00"), marker...)
	seg := make([]byte, 0, 4+len(payload))
	seg = append(seg, 0xFF, 0xE1, byte((len(payload)+2)>>8), byte(len(payload)+2))
	seg = append(seg, payload...)
	data := buf.Bytes()
	return append(append(append([]byte{}, data[:2]...), seg...), data[2:]...)
}

func failure(t *testing.T, err error) string {
	t.Helper()
	f, ok := imaging.AsFailure(err)
	require.True(t, ok, "%v", err)
	return f.Reason
}

func TestInspect(t *testing.T) {
	info, err := imaging.Inspect(pngOf(t, 300, 200), limits)
	require.NoError(t, err)
	assert.Equal(t, imaging.Info{ContentType: imaging.TypePNG, Width: 300, Height: 200}, info)

	_, err = imaging.Inspect([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), limits)
	assert.Equal(t, imaging.ReasonUnsupportedFormat, failure(t, err))

	_, err = imaging.Inspect(pngOf(t, 10, 10)[:30], limits)
	assert.Equal(t, imaging.ReasonCorrupt, failure(t, err))

	_, err = imaging.Inspect(nil, limits)
	assert.Equal(t, imaging.ReasonCorrupt, failure(t, err))

	small := limits
	small.MaxBytes = 100
	_, err = imaging.Inspect(pngOf(t, 50, 50), small)
	assert.Equal(t, imaging.ReasonTooLarge, failure(t, err))

	_, err = imaging.Inspect(pngOf(t, 1100, 100), limits)
	assert.Equal(t, imaging.ReasonBadDimensions, failure(t, err), "aspect ratio")

	narrow := limits
	narrow.MaxDimension = 100
	_, err = imaging.Inspect(pngOf(t, 101, 100), narrow)
	assert.Equal(t, imaging.ReasonBadDimensions, failure(t, err))
}

func decode(t *testing.T, o imaging.Output) image.Image {
	t.Helper()
	var (
		img image.Image
		err error
	)
	switch o.Format {
	case imaging.FormatWebP:
		img, err = webp.Decode(bytes.NewReader(o.Data))
	case imaging.FormatJPEG:
		img, err = jpeg.Decode(bytes.NewReader(o.Data))
	case imaging.FormatPNG:
		img, err = png.Decode(bytes.NewReader(o.Data))
	}
	require.NoError(t, err, "%s %s", o.Variant, o.Format)
	return img
}

func TestRender(t *testing.T) {
	outs, err := imaging.Render(pngOf(t, 3000, 1500), imaging.Quality{JPEG: 85, WebP: 80})
	require.NoError(t, err)

	type key struct{ variant, format string }
	got := map[key][2]int{}
	for _, o := range outs {
		b := decode(t, o).Bounds()
		assert.Equal(t, [2]int{o.Width, o.Height}, [2]int{b.Dx(), b.Dy()}, "%s %s", o.Variant, o.Format)
		got[key{o.Variant, o.Format}] = [2]int{o.Width, o.Height}
	}
	assert.Equal(t, map[key][2]int{
		{"original", "png"}:   {3000, 1500},
		{"xlarge", "webp"}:    {2048, 1024},
		{"xlarge", "jpeg"}:    {2048, 1024},
		{"large", "webp"}:     {1024, 512},
		{"large", "jpeg"}:     {1024, 512},
		{"medium", "webp"}:    {512, 256},
		{"medium", "jpeg"}:    {512, 256},
		{"thumbnail", "webp"}: {200, 100},
		{"thumbnail", "jpeg"}: {200, 100},
	}, got)
}

func TestRenderDoesNotEnlarge(t *testing.T) {
	outs, err := imaging.Render(pngOf(t, 150, 300), imaging.Quality{JPEG: 85, WebP: 80})
	require.NoError(t, err)
	for _, o := range outs {
		want := [2]int{150, 300}
		if o.Variant == "thumbnail" {
			want = [2]int{100, 200}
		}
		assert.Equal(t, want, [2]int{o.Width, o.Height}, "%s %s", o.Variant, o.Format)
	}
}

func TestRenderStripsExif(t *testing.T) {
	const marker = "GPS-SECRET-LOCATION"
	src := jpegWithExif(t, marker)
	require.Contains(t, string(src), marker)

	outs, err := imaging.Render(src, imaging.Quality{JPEG: 85, WebP: 80})
	require.NoError(t, err)
	assert.Equal(t, imaging.FormatJPEG, outs[0].Format, "a JPEG original stays JPEG")
	for _, o := range outs {
		assert.NotContains(t, string(o.Data), marker, "%s %s", o.Variant, o.Format)
	}
}
