// Package domain holds yc-product's catalog types and their invariants. It
// does no I/O.
package domain

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is the lifecycle of suppliers, products and SKUs. Deleted is set
// by soft deletes only; clients send active or inactive.
type Status string

// Statuses.
const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
	StatusDeleted  Status = "deleted"
)

// Settable reports whether clients may set s.
func (s Status) Settable() bool { return s == StatusActive || s == StatusInactive }

// Field limits, mirrored in api/openapi.yaml.
const (
	MaxNameLen        = 200
	MaxDescriptionLen = 10000
	MaxCategories     = 20
	MaxAxisValues     = 100
	MaxValueLen       = 100
	MaxSKUCodeLen     = 64
	MaxSlugLen        = 100
	MaxCategoryName   = 100
	MaxAttributes     = 50
	MaxContactFields  = 20
	MaxQueryLen       = 200
)

var (
	axisRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	taxClassRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Supplier supplies a shop's products. System admins manage suppliers.
type Supplier struct {
	ID        string
	TenantID  string
	Name      string
	Contact   map[string]string
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SupplierInput is the settable part of a Supplier.
type SupplierInput struct {
	Name    string
	Contact map[string]string
	Status  Status // "" means active
}

// Validate normalises in and reports invalid fields.
func (in *SupplierInput) Validate() error {
	v := ycerrors.Validation()
	in.Name = strings.TrimSpace(in.Name)
	checkText(v, "name", in.Name, MaxNameLen, true)
	if len(in.Contact) > MaxContactFields {
		v.Field("contact", "too_many", fmt.Sprintf("At most %d contact fields", MaxContactFields))
	}
	if in.Contact == nil {
		in.Contact = map[string]string{}
	}
	checkStatus(v, &in.Status)
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Product is a conceptual item ("Acme T-Shirt"); its SKUs are what sells.
type Product struct {
	ID          string
	TenantID    string
	SupplierID  string // "" when none
	Name        string
	Description string
	IsDigital   bool
	Status      Status
	Attributes  map[string]string
	CategoryIDs []string
	Variants    []Variant
	SKUs        []SKU
	Images      []Image
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProductInput is the settable part of a Product. Updates replace every
// field.
type ProductInput struct {
	Name        string
	Description string
	SupplierID  string
	IsDigital   bool
	Status      Status // "" means active
	CategoryIDs []string
	Attributes  map[string]string
}

// Validate normalises in and reports invalid fields.
func (in *ProductInput) Validate() error {
	v := ycerrors.Validation()
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	checkText(v, "name", in.Name, MaxNameLen, true)
	checkText(v, "description", in.Description, MaxDescriptionLen, false)
	checkStatus(v, &in.Status)
	if len(in.CategoryIDs) > MaxCategories {
		v.Field("category_ids", "too_many", fmt.Sprintf("At most %d categories", MaxCategories))
	}
	slices.Sort(in.CategoryIDs)
	in.CategoryIDs = slices.Compact(in.CategoryIDs)
	if in.CategoryIDs == nil {
		in.CategoryIDs = []string{}
	}
	if len(in.Attributes) > MaxAttributes {
		v.Field("attributes", "too_many", fmt.Sprintf("At most %d attributes", MaxAttributes))
	}
	if in.Attributes == nil {
		in.Attributes = map[string]string{}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Variant is an axis of variation ("size": S, M, L).
type Variant struct {
	ID        string
	ProductID string
	AxisName  string
	Values    []string
	CreatedAt time.Time
}

// VariantInput is a new variant axis.
type VariantInput struct {
	AxisName string
	Values   []string
}

// Validate normalises in and reports invalid fields.
func (in *VariantInput) Validate() error {
	v := ycerrors.Validation()
	if !axisRe.MatchString(in.AxisName) {
		v.Field("axis_name", "invalid", "Lower case letters, digits and underscores, starting with a letter")
	}
	switch {
	case len(in.Values) == 0:
		v.Field("values", "required", "At least one value is required")
	case len(in.Values) > MaxAxisValues:
		v.Field("values", "too_many", fmt.Sprintf("At most %d values", MaxAxisValues))
	}
	seen := map[string]bool{}
	for i, val := range in.Values {
		val = strings.TrimSpace(val)
		in.Values[i] = val
		field := fmt.Sprintf("values[%d]", i)
		switch {
		case val == "" || utf8.RuneCountInString(val) > MaxValueLen:
			v.Field(field, "invalid", fmt.Sprintf("1 to %d characters", MaxValueLen))
		case seen[val]:
			v.Field(field, "duplicate", "Values must be unique")
		}
		seen[val] = true
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// SKU is a sellable product × variant combination.
type SKU struct {
	ID               string
	TenantID         string
	ProductID        string
	Code             string
	VariantValues    map[string]string
	InventoryTracked bool
	TaxClass         string
	WeightGrams      *int
	Status           Status
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SKUInput is the settable part of a SKU. Updates replace every field.
type SKUInput struct {
	Code             string
	VariantValues    map[string]string
	InventoryTracked bool
	TaxClass         string
	WeightGrams      *int
	Status           Status // "" means active
}

// Validate normalises in and reports invalid fields. CheckVariants checks
// the variant values against the product.
func (in *SKUInput) Validate() error {
	v := ycerrors.Validation()
	in.Code = strings.TrimSpace(in.Code)
	checkText(v, "sku_code", in.Code, MaxSKUCodeLen, true)
	if !taxClassRe.MatchString(in.TaxClass) {
		v.Field("tax_class", "invalid", "A yc-pricing tax class ID, e.g. za-standard")
	}
	if in.WeightGrams != nil && *in.WeightGrams < 0 {
		v.Field("weight_grams", "invalid", "Must not be negative")
	}
	checkStatus(v, &in.Status)
	if in.VariantValues == nil {
		in.VariantValues = map[string]string{}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// CheckVariants reports whether values names exactly one allowed value on
// every axis of variants.
func CheckVariants(variants []Variant, values map[string]string) error {
	v := ycerrors.Validation()
	axes := map[string][]string{}
	for _, va := range variants {
		axes[va.AxisName] = va.Values
	}
	for _, axis := range slices.Sorted(maps.Keys(values)) {
		allowed, ok := axes[axis]
		switch {
		case !ok:
			v.Field("variant_values."+axis, "unknown_axis", "The product has no such variant axis")
		case !slices.Contains(allowed, values[axis]):
			v.Field("variant_values."+axis, "invalid", "Not one of the axis values: "+strings.Join(allowed, ", "))
		}
	}
	for _, va := range variants {
		if _, ok := values[va.AxisName]; !ok {
			v.Field("variant_values."+va.AxisName, "required", "A value is required on every variant axis")
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Image is a yc-image image attached to a product.
type Image struct {
	ImageID   string
	SortOrder int
	IsPrimary bool
}

// Category is a node in a shop's category tree.
type Category struct {
	ID        string
	ParentID  string // "" for a root
	Name      string
	Slug      string
	SortOrder int
}

// CategoryInput is the settable part of a Category.
type CategoryInput struct {
	ParentID  string
	Name      string
	Slug      string
	SortOrder int
}

// Validate normalises in and reports invalid fields.
func (in *CategoryInput) Validate() error {
	v := ycerrors.Validation()
	in.Name = strings.TrimSpace(in.Name)
	checkText(v, "name", in.Name, MaxCategoryName, true)
	if !slugRe.MatchString(in.Slug) || len(in.Slug) > MaxSlugLen {
		v.Field("slug", "invalid", "Lower case letters and digits separated by single hyphens")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// CategoryNode is a category with its subcategories.
type CategoryNode struct {
	Category
	Children []*CategoryNode
}

// BuildTree arranges categories into trees, siblings by sort order then
// name. A category whose parent is missing becomes a root.
func BuildTree(cats []Category) []*CategoryNode {
	nodes := make(map[string]*CategoryNode, len(cats))
	for _, c := range cats {
		nodes[c.ID] = &CategoryNode{Category: c}
	}
	var roots []*CategoryNode
	for _, c := range cats {
		n := nodes[c.ID]
		if p, ok := nodes[c.ParentID]; ok && c.ParentID != "" {
			p.Children = append(p.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	var sortAll func([]*CategoryNode)
	sortAll = func(ns []*CategoryNode) {
		sort.SliceStable(ns, func(i, j int) bool {
			if ns[i].SortOrder != ns[j].SortOrder {
				return ns[i].SortOrder < ns[j].SortOrder
			}
			return ns[i].Name < ns[j].Name
		})
		for _, n := range ns {
			sortAll(n.Children)
		}
	}
	sortAll(roots)
	return roots
}

// ProductSummary is a product in a browse result.
type ProductSummary struct {
	ID             string
	SupplierID     string
	Name           string
	Description    string
	IsDigital      bool
	CategoryIDs    []string
	PrimaryImageID string
	SKUs           []SKU
	CreatedAt      time.Time
}

// BrowseFilter narrows GET /products.
type BrowseFilter struct {
	Query      string
	CategoryID string
	SupplierID string
}

// Validate normalises f.
func (f *BrowseFilter) Validate() error {
	f.Query = strings.TrimSpace(f.Query)
	if utf8.RuneCountInString(f.Query) > MaxQueryLen {
		return ycerrors.Validation().Field("q", "too_long", fmt.Sprintf("At most %d characters", MaxQueryLen)).Build()
	}
	return nil
}

func checkText(v *ycerrors.ValidationBuilder, field, s string, maxLen int, required bool) {
	n := utf8.RuneCountInString(s)
	switch {
	case required && n == 0:
		v.Field(field, "required", "Required")
	case n > maxLen:
		v.Field(field, "too_long", fmt.Sprintf("At most %d characters", maxLen))
	}
}

func checkStatus(v *ycerrors.ValidationBuilder, s *Status) {
	if *s == "" {
		*s = StatusActive
	}
	if !s.Settable() {
		v.Field("status", "invalid", "active or inactive")
	}
}
