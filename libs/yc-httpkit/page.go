package httpkit

import (
	"encoding/base64"
	"encoding/json"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Page sizes (api-conventions.md §7).
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// PageRequest is a validated page_size and cursor.
type PageRequest struct {
	Size   int
	Cursor string
}

// ParsePage validates the page_size and cursor query parameters, which
// generated servers pass as pointers. A nil size means DefaultPageSize.
func ParsePage(size *int, cursor *string) (PageRequest, error) {
	p := PageRequest{Size: DefaultPageSize}
	if size != nil {
		if *size < 1 || *size > MaxPageSize {
			return PageRequest{}, ycerrors.Validation().
				Field("page_size", "out_of_range", "page_size must be between 1 and 100").Build()
		}
		p.Size = *size
	}
	if cursor != nil {
		p.Cursor = *cursor
	}
	return p, nil
}

// PageInfo is the page object of a collection response. Specs map their
// Page schema to it with x-go-type.
type PageInfo struct {
	Size           int    `json:"size"`
	Cursor         string `json:"cursor,omitempty"`
	NextCursor     string `json:"next_cursor,omitempty"`
	TotalAvailable *int64 `json:"total_available,omitempty"`
}

// EncodeCursor returns an opaque cursor holding v, typically the sort key
// of the last item returned.
func EncodeCursor(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("httpkit: cursor value is not JSON-encodable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reads a cursor made by EncodeCursor into v. A malformed
// cursor is a validation error.
func DecodeCursor(cursor string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return ycerrors.Validation().Field("cursor", "invalid", "Cursor is not valid").Wrap(err)
	}
	return nil
}
