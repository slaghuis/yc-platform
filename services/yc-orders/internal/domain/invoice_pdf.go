package domain

import (
	"bytes"
	"fmt"
	"strings"
)

// InvoicePDF renders a simple text PDF for an order (issue #29).
// Amounts are taken from the order snapshot only — no tax recalculation.
func InvoicePDF(o Order) []byte {
	title := "Invoice"
	if o.SellerInvoice != nil && o.SellerInvoice.VATNumber != "" {
		title = "Tax Invoice"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "Invoice number: %s\n", o.OrderNumber)
	if o.PlacedAt != nil {
		fmt.Fprintf(&b, "Invoice date: %s\n", o.PlacedAt.UTC().Format("2006-01-02"))
	} else {
		fmt.Fprintf(&b, "Invoice date: %s\n", o.CreatedAt.UTC().Format("2006-01-02"))
	}
	b.WriteString("\nSupplier\n")
	if s := o.SellerInvoice; s != nil {
		fmt.Fprintf(&b, "%s\n%s\n", s.Name, s.ContactEmail)
		if s.VATNumber != "" {
			fmt.Fprintf(&b, "VAT: %s\n", s.VATNumber)
		}
		if a := s.InvoiceAddress; a != nil {
			writeAddress(&b, *a)
		}
	}
	b.WriteString("\nRecipient\n")
	if a := o.BillingAddress; a != nil {
		if a.CompanyName != "" {
			fmt.Fprintf(&b, "%s\n", a.CompanyName)
		}
		if a.VATNumber != "" {
			fmt.Fprintf(&b, "VAT: %s\n", a.VATNumber)
		}
		writeAddress(&b, *a)
	} else if g := o.GuestContact; g != nil {
		fmt.Fprintf(&b, "%s\n", g.Email)
	} else {
		fmt.Fprintf(&b, "Customer %s\n", o.UserID)
	}
	b.WriteString("\nLines\n")
	for _, it := range o.Items {
		fmt.Fprintf(&b, "%s  qty %d  %s\n", it.NameSnapshot, it.Quantity, money(it.LineTotalMinor, o.Currency))
	}
	b.WriteString("\nTotals\n")
	fmt.Fprintf(&b, "Items: %s\n", money(o.ItemsTotalMinor, o.Currency))
	fmt.Fprintf(&b, "Shipping: %s\n", money(o.ShippingMinor, o.Currency))
	fmt.Fprintf(&b, "Tax: %s\n", money(o.TaxTotalMinor, o.Currency))
	fmt.Fprintf(&b, "Total: %s\n", money(o.TotalMinor, o.Currency))
	return simplePDF(b.String())
}

func writeAddress(b *strings.Builder, a BillingAddress) {
	fmt.Fprintf(b, "%s\n", a.StreetAndNumber)
	if a.Suburb != "" {
		fmt.Fprintf(b, "%s\n", a.Suburb)
	}
	fmt.Fprintf(b, "%s\n%s\n%s\n", a.TownCity, a.PostalCode, a.CountryCode)
}

func money(minor int64, currency string) string {
	neg := ""
	if minor < 0 {
		neg, minor = "-", -minor
	}
	return fmt.Sprintf("%s%s %d.%02d", neg, currency, minor/100, minor%100)
}

// simplePDF builds a minimal single-page PDF with Helvetica text lines.
func simplePDF(text string) []byte {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var content strings.Builder
	content.WriteString("BT /F1 10 Tf 50 780 Td 14 TL\n")
	for i, line := range lines {
		if i > 0 {
			content.WriteString("T*\n")
		}
		content.WriteString("(")
		content.WriteString(pdfEscape(line))
		content.WriteString(") Tj\n")
	}
	content.WriteString("ET\n")
	stream := content.String()

	var out bytes.Buffer
	w := func(s string) { out.WriteString(s) }
	offsets := make([]int, 6)
	w("%PDF-1.4\n")
	writeObj := func(n int, body string) {
		offsets[n] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	writeObj(4, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	writeObj(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes()
}

func pdfEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)", "\r", " ")
	return r.Replace(s)
}
