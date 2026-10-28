package domain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

func fields(t *testing.T, err error) []string {
	t.Helper()
	var e *ycerrors.Error
	require.ErrorAs(t, err, &e)
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Field + ":" + f.Code
	}
	return out
}

func TestCheckVariants(t *testing.T) {
	variants := []domain.Variant{
		{AxisName: "colour", Values: []string{"Red", "Blue"}},
		{AxisName: "size", Values: []string{"S", "M"}},
	}
	require.NoError(t, domain.CheckVariants(variants, map[string]string{"colour": "Red", "size": "M"}))
	require.NoError(t, domain.CheckVariants(nil, map[string]string{}))

	err := domain.CheckVariants(variants, map[string]string{"colour": "Green", "fit": "slim"})
	assert.Equal(t, []string{
		"variant_values.colour:invalid", "variant_values.fit:unknown_axis", "variant_values.size:required",
	}, fields(t, err))
}

func TestProductInputValidate(t *testing.T) {
	in := domain.ProductInput{Name: "  Mug  ", CategoryIDs: []string{"b", "a", "b"}}
	require.NoError(t, in.Validate())
	assert.Equal(t, "Mug", in.Name)
	assert.Equal(t, domain.StatusActive, in.Status)
	assert.Equal(t, []string{"a", "b"}, in.CategoryIDs)
	assert.NotNil(t, in.Attributes)

	bad := domain.ProductInput{Name: " ", Status: domain.StatusDeleted}
	assert.Equal(t, []string{"name:required", "status:invalid"}, fields(t, bad.Validate()))
}

func TestVariantInputValidate(t *testing.T) {
	in := domain.VariantInput{AxisName: "Colour", Values: []string{"Red", " Red ", ""}}
	assert.Equal(t, []string{"axis_name:invalid", "values[1]:duplicate", "values[2]:invalid"}, fields(t, in.Validate()))
}

func TestSKUInputValidate(t *testing.T) {
	w := -1
	in := domain.SKUInput{Code: "", TaxClass: "VAT 15", WeightGrams: &w}
	assert.Equal(t, []string{"sku_code:required", "tax_class:invalid", "weight_grams:invalid"}, fields(t, in.Validate()))

	box := domain.SKUInput{Code: "MUG", TaxClass: "za-standard", Dimensions: &domain.Dimensions{LengthMM: 0, WidthMM: 90, HeightMM: 10_001}}
	assert.Equal(t, []string{"dimensions_mm.length:invalid", "dimensions_mm.height:invalid"}, fields(t, box.Validate()))
	box.Dimensions = &domain.Dimensions{LengthMM: 120, WidthMM: 90, HeightMM: 100}
	require.NoError(t, box.Validate())

	ok := domain.SKUInput{Code: " TS-1 ", TaxClass: "za-standard"}
	require.NoError(t, ok.Validate())
	assert.Equal(t, "TS-1", ok.Code)
	assert.NotNil(t, ok.VariantValues)
}

func TestBannerInputValidate(t *testing.T) {
	in := domain.BannerInput{
		CategoryID:        "11111111-1111-4111-8111-111111111111",
		Text:              "  New in  ",
		BackgroundImageID: "22222222-2222-4222-8222-222222222222",
		LargeImageID:      "33333333-3333-4333-8333-333333333333",
	}
	require.NoError(t, in.Validate())
	assert.Equal(t, "New in", in.Text)

	bad := domain.BannerInput{CategoryID: "nope", Text: " ", BackgroundImageID: "x", LargeImageID: "y"}
	assert.Equal(t, []string{
		"category_id:invalid", "text:required", "background_image_id:invalid", "large_image_id:invalid",
	}, fields(t, bad.Validate()))

	too := in
	too.Text = strings.Repeat("a", domain.MaxBannerText+1)
	assert.Equal(t, []string{"text:too_long"}, fields(t, too.Validate()))

	assert.Equal(t, []string{"ids[1]:duplicate"}, fields(t, domain.ValidateBannerOrder([]string{in.CategoryID, in.CategoryID})))
	require.NoError(t, domain.ValidateBannerOrder(nil))
}

func TestBrowseFilterValidate(t *testing.T) {
	vegan := "11111111-1111-4111-8111-111111111111"
	nuts := "22222222-2222-4222-8222-222222222222"
	f := domain.BrowseFilter{
		CategoryIDs:        []string{vegan, vegan, "  " + nuts + "  "},
		ExcludeCategoryIDs: []string{nuts},
	}
	require.NoError(t, f.Validate())
	assert.Equal(t, []string{vegan, nuts}, f.CategoryIDs)
	assert.Equal(t, []string{nuts}, f.ExcludeCategoryIDs)

	bad := domain.BrowseFilter{CategoryIDs: []string{"not-a-uuid"}}
	assert.Equal(t, []string{"category:invalid"}, fields(t, bad.Validate()))

	tooMany := make([]string, domain.MaxBrowseCategoryFilters+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
	}
	assert.Equal(t, []string{"category:too_many"}, fields(t, (&domain.BrowseFilter{CategoryIDs: tooMany}).Validate()))
}

func TestValidateRelatedIDs(t *testing.T) {
	self := "00000000-0000-4000-8000-000000000001"
	a := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	got, err := domain.ValidateRelatedIDs(self, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{}, got, "an empty list clears")

	got, err = domain.ValidateRelatedIDs(self, []string{b, a, strings.ToUpper(b), " " + a + " "})
	require.NoError(t, err)
	assert.Equal(t, []string{b, a}, got, "duplicates collapse to their first place; order is kept")

	_, err = domain.ValidateRelatedIDs(self, []string{a, "not-a-uuid", strings.ToUpper(self)})
	assert.Equal(t, []string{"product_ids[1]:invalid", "product_ids[2]:self"}, fields(t, err))

	max := make([]string, domain.MaxRelatedProducts)
	for i := range max {
		max[i] = fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
	}
	got, err = domain.ValidateRelatedIDs(self, max)
	require.NoError(t, err)
	assert.Len(t, got, domain.MaxRelatedProducts)

	over := append(append([]string{}, max...), a)
	_, err = domain.ValidateRelatedIDs(self, over)
	assert.Equal(t, []string{"product_ids:too_many"}, fields(t, err))

	got, err = domain.ValidateRelatedIDs(self, append(append([]string{}, max...), max...))
	require.NoError(t, err, "the cap counts distinct ids")
	assert.Len(t, got, domain.MaxRelatedProducts)
}

func TestValidateAlsoBoughtLimit(t *testing.T) {
	for _, ok := range []int{1, domain.DefaultAlsoBought, domain.MaxAlsoBought} {
		assert.NoError(t, domain.ValidateAlsoBoughtLimit(ok), ok)
	}
	for _, bad := range []int{-1, 0, domain.MaxAlsoBought + 1} {
		assert.Equal(t, []string{"limit:invalid"}, fields(t, domain.ValidateAlsoBoughtLimit(bad)), bad)
	}
}

func TestBuildTree(t *testing.T) {
	roots := domain.BuildTree([]domain.Category{
		{ID: "c", ParentID: "a", Name: "Zeta", SortOrder: 0},
		{ID: "a", Name: "Apparel", SortOrder: 2},
		{ID: "b", Name: "Books", SortOrder: 1},
		{ID: "d", ParentID: "a", Name: "Alpha", SortOrder: 0},
		{ID: "e", ParentID: "missing", Name: "Orphan", SortOrder: 3},
	})
	names := func(ns []*domain.CategoryNode) []string {
		out := make([]string, 0, len(ns))
		for _, n := range ns {
			out = append(out, n.Name)
		}
		return out
	}
	assert.Equal(t, []string{"Books", "Apparel", "Orphan"}, names(roots))
	assert.Equal(t, []string{"Alpha", "Zeta"}, names(roots[1].Children))
}

func TestReviewStats(t *testing.T) {
	none := domain.ReviewStats{LikeCount: 3}
	assert.Nil(t, none.Average())
	assert.Equal(t, int64(-1), none.RatingKey(), "unrated sorts last")
	require.NoError(t, none.Validate())

	s := domain.ReviewStats{RatingCount: 3, RatingSum: 13}
	require.NotNil(t, s.Average())
	assert.InDelta(t, 4.333, *s.Average(), 0.001)
	assert.Equal(t, int64(4333), s.RatingKey())
	require.NoError(t, s.Validate())

	assert.Equal(t, []string{"product_stats:invalid"}, fields(t, domain.ReviewStats{RatingCount: 1, RatingSum: 6}.Validate()))
	assert.Equal(t, []string{"product_stats:invalid"}, fields(t, domain.ReviewStats{RatingCount: 2, RatingSum: 1}.Validate()))
	assert.Equal(t, []string{"product_stats:invalid"}, fields(t, domain.ReviewStats{LikeCount: -1}.Validate()))
}
