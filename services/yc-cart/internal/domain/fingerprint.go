package domain

import (
	"fmt"
	"sort"
	"strings"
)

// Fingerprint is a stable digest of cart lines (sku_id:qty) used to decide
// whether a recovery follow-up may still fire (issue #35).
func Fingerprint(items []Item) string {
	if len(items) == 0 {
		return ""
	}
	type line struct {
		sku string
		qty int
	}
	lines := make([]line, len(items))
	for i, it := range items {
		lines[i] = line{sku: it.SKUID, qty: it.Quantity}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].sku < lines[j].sku })
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf("%s:%d", l.sku, l.qty)
	}
	return strings.Join(parts, "|")
}
