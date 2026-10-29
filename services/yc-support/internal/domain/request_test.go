package domain_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
)

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func TestStatusAfterPost(t *testing.T) {
	cases := []struct {
		name   string
		from   domain.Status
		author domain.AuthorType
		want   domain.Status
	}{
		{"shop answers new", domain.StatusNew, domain.AuthorShopAdmin, domain.StatusOpenCustomer},
		{"requester follows up before an answer", domain.StatusNew, domain.AuthorCustomer, domain.StatusNew},
		{"customer replies", domain.StatusOpenCustomer, domain.AuthorCustomer, domain.StatusOpenShop},
		{"visitor replies", domain.StatusOpenCustomer, domain.AuthorVisitor, domain.StatusOpenShop},
		{"shop writes again", domain.StatusOpenCustomer, domain.AuthorShopAdmin, domain.StatusOpenCustomer},
		{"shop answers reply", domain.StatusOpenShop, domain.AuthorShopAdmin, domain.StatusOpenCustomer},
		{"customer adds to reply", domain.StatusOpenShop, domain.AuthorCustomer, domain.StatusOpenShop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := domain.StatusAfterPost(c.from, c.author)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestStatusAfterPostClosedIsConflict(t *testing.T) {
	for _, a := range []domain.AuthorType{domain.AuthorCustomer, domain.AuthorVisitor, domain.AuthorShopAdmin} {
		_, err := domain.StatusAfterPost(domain.StatusClosed, a)
		assert.True(t, ycerrors.HasCode(err, ycerrors.CodeConflict), "author %s", a)
	}
}

func TestStatusAfterPostRejectsSystemAndUnknown(t *testing.T) {
	_, err := domain.StatusAfterPost(domain.StatusNew, domain.AuthorSystem)
	assert.Error(t, err)
	_, err = domain.StatusAfterPost("bogus", domain.AuthorCustomer)
	assert.Error(t, err)
}

func TestRequestPostAndClose(t *testing.T) {
	r := domain.Request{Status: domain.StatusNew, LastActivityAt: t0.Add(-time.Hour)}
	require.NoError(t, r.Post(domain.AuthorShopAdmin, t0))
	assert.Equal(t, domain.StatusOpenCustomer, r.Status)
	assert.Equal(t, t0, r.LastActivityAt)

	later := t0.Add(time.Minute)
	require.NoError(t, r.Close(domain.ClosedStaff, later))
	assert.Equal(t, domain.StatusClosed, r.Status)
	assert.Equal(t, domain.ClosedStaff, r.ClosedReason)
	require.NotNil(t, r.ClosedAt)
	assert.Equal(t, later, *r.ClosedAt)

	// No reopen, no second close.
	assert.True(t, ycerrors.HasCode(r.Post(domain.AuthorCustomer, later), ycerrors.CodeConflict))
	assert.True(t, ycerrors.HasCode(r.Close(domain.ClosedStaff, later), ycerrors.CodeConflict))
	assert.Equal(t, domain.StatusClosed, r.Status)
}

func TestIdleSince(t *testing.T) {
	ttl := 72 * time.Hour
	open := domain.Request{Status: domain.StatusOpenShop, LastActivityAt: t0.Add(-ttl)}
	assert.True(t, open.IdleSince(t0, ttl), "exactly 72h counts as idle")
	open.LastActivityAt = t0.Add(-ttl + time.Second)
	assert.False(t, open.IdleSince(t0, ttl))
	closed := domain.Request{Status: domain.StatusClosed, LastActivityAt: t0.Add(-100 * time.Hour)}
	assert.False(t, closed.IdleSince(t0, ttl))
}

func TestOwnedByAndNotifiable(t *testing.T) {
	r := domain.Request{UserID: "u1", ContactEmail: "a@b.co"}
	assert.True(t, r.OwnedBy("u1"))
	assert.False(t, r.OwnedBy("u2"))
	assert.False(t, r.OwnedBy(""))
	assert.False(t, domain.Request{}.OwnedBy(""), "an empty owner never matches an empty caller")
	assert.True(t, r.Notifiable())
	redacted := t0
	r.RedactedAt = &redacted
	assert.False(t, r.Notifiable())
	assert.False(t, domain.Request{}.Notifiable())
}

func TestNormaliseEmail(t *testing.T) {
	got, err := domain.NormaliseEmail("  Jane.Doe@Example.COM ")
	require.NoError(t, err)
	assert.Equal(t, "jane.doe@example.com", got)

	for _, bad := range []string{"", "plain", "a@b", "Jane <jane@example.com>", "a@b@c.com", "@x.com", strings.Repeat("a", 330) + "@x.com"} {
		_, err := domain.NormaliseEmail(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestNewRequestValidate(t *testing.T) {
	good := func() domain.NewRequest {
		return domain.NewRequest{
			Topic: domain.TopicAllergen, ContactEmail: " Jane@Example.com ", ContactName: " Jane ",
			ContactPhone: "+27821234567", Body: "  Does the cake contain nuts?  ",
		}
	}
	n := good()
	require.NoError(t, n.Validate())
	assert.Equal(t, "jane@example.com", n.ContactEmail)
	assert.Equal(t, "Jane", n.ContactName)
	assert.Equal(t, "Does the cake contain nuts?", n.Body)

	fields := func(mutate func(*domain.NewRequest)) []string {
		n := good()
		mutate(&n)
		err := n.Validate()
		require.Error(t, err)
		e, ok := ycerrors.As(err)
		require.True(t, ok)
		out := make([]string, 0, len(e.Fields))
		for _, f := range e.Fields {
			out = append(out, f.Field)
		}
		return out
	}
	assert.Equal(t, []string{"topic"}, fields(func(n *domain.NewRequest) { n.Topic = "refund" }))
	assert.Equal(t, []string{"contact_email"}, fields(func(n *domain.NewRequest) { n.ContactEmail = "nope" }))
	assert.Equal(t, []string{"contact_name"}, fields(func(n *domain.NewRequest) { n.ContactName = " " }))
	assert.Equal(t, []string{"contact_phone"}, fields(func(n *domain.NewRequest) { n.ContactPhone = "0821234567" }))
	assert.Equal(t, []string{"body"}, fields(func(n *domain.NewRequest) { n.Body = "   " }))
	assert.Equal(t, []string{"body"}, fields(func(n *domain.NewRequest) { n.Body = strings.Repeat("é", domain.MaxBodyLength+1) }))
	assert.Equal(t, []string{"topic", "body"}, fields(func(n *domain.NewRequest) { n.Topic, n.Body = "", "" }))

	n = good()
	n.ContactPhone = ""
	assert.NoError(t, n.Validate(), "phone is optional")
	n = good()
	n.Body = strings.Repeat("é", domain.MaxBodyLength)
	assert.NoError(t, n.Validate(), "the limit counts characters, not bytes")
}

func TestValidateBody(t *testing.T) {
	got, err := domain.ValidateBody("  hi ")
	require.NoError(t, err)
	assert.Equal(t, "hi", got)
	_, err = domain.ValidateBody("\n")
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))
}

func TestExcerpt(t *testing.T) {
	assert.Equal(t, "a b c", domain.Excerpt("a\n  b\tc "))
	long := strings.Repeat("word ", 100)
	got := domain.Excerpt(long)
	assert.LessOrEqual(t, len([]rune(got)), domain.MaxExcerpt)
	assert.True(t, strings.HasSuffix(got, "…"))
}

func TestNewReference(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		ref, err := domain.NewReference(nil)
		require.NoError(t, err)
		assert.True(t, domain.ValidReference(ref), ref)
		seen[ref] = true
	}
	assert.Greater(t, len(seen), 190, "references are random")
}

func TestNewReferenceDeterministicSource(t *testing.T) {
	// Bytes of 240 and above are skipped; 0, 1, 2 ... pick the alphabet in order.
	src := bytes.NewReader([]byte{255, 0, 1, 2, 250, 3, 4, 5})
	ref, err := domain.NewReference(src)
	require.NoError(t, err)
	assert.Equal(t, "SUP-234567", ref)

	_, err = domain.NewReference(bytes.NewReader([]byte{1, 2}))
	assert.Error(t, err, "a short source fails instead of repeating")
}

func TestValidReference(t *testing.T) {
	assert.True(t, domain.ValidReference("SUP-7K3M9Q"))
	assert.False(t, domain.ValidReference("SUP-7K3M9"))
	assert.False(t, domain.ValidReference("sup-7k3m9q"))
	assert.False(t, domain.ValidReference("SUP-0O1I2L"), "ambiguous characters are never issued")
}

func TestTopics(t *testing.T) {
	for _, topic := range domain.Topics() {
		assert.True(t, topic.Valid())
	}
	assert.Len(t, domain.Topics(), 6)
	assert.False(t, domain.Topic("").Valid())
}
