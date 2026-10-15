package domain_test

import (
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
