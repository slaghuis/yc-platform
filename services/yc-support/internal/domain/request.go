package domain

import (
	"crypto/rand"
	"errors"
	"io"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Limits on what a requester may submit.
const (
	MaxBodyLength    = 5000
	MaxNameLength    = 200
	MaxEmailLength   = 320
	MaxPageURLLength = 2048
	MaxExcerpt       = 200

	// ReferencePrefix starts every reference.
	ReferencePrefix = "SUP-"
	referenceLength = 6
	// referenceAlphabet leaves out 0, 1, I, L, O and U, which are easily
	// misread or mistyped when a customer quotes a reference.
	referenceAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var (
	phonePattern     = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	referencePattern = regexp.MustCompile(`^SUP-[` + referenceAlphabet + `]{6}$`)
)

// Request is a support thread's header.
type Request struct {
	ID        string
	TenantID  string
	Reference string
	Topic     Topic
	Status    Status

	ContactEmail string
	ContactName  string
	ContactPhone string

	// UserID is empty until a customer opens the request or claims it.
	UserID   string
	UserType UserType
	OrderID  string
	PageURL  string

	LastActivityAt time.Time
	ClosedAt       *time.Time
	ClosedReason   ClosedReason
	// RedactedAt is set when the requester's data was erased (GDPR).
	RedactedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Message is one post on a thread. Messages are never edited or deleted.
type Message struct {
	ID           string
	RequestID    string
	AuthorType   AuthorType
	AuthorUserID string
	Body         string
	CreatedAt    time.Time
}

// Thread is a request with its messages, oldest first.
type Thread struct {
	Request  Request
	Messages []Message
}

// Notifiable reports whether the requester can still be written to: they
// have an account or a contact email.
func (r Request) Notifiable() bool {
	return r.RedactedAt == nil && (r.UserID != "" || r.ContactEmail != "")
}

// OwnedBy reports whether userID is the customer the request belongs to.
func (r Request) OwnedBy(userID string) bool {
	return userID != "" && r.UserID == userID
}

// IdleSince reports whether the request is open and has had no message
// since now-ttl.
func (r Request) IdleSince(now time.Time, ttl time.Duration) bool {
	return !r.Status.Closed() && !r.LastActivityAt.After(now.Add(-ttl))
}

// Close marks the request closed. It fails with a conflict if it already is.
func (r *Request) Close(reason ClosedReason, now time.Time) error {
	if r.Status.Closed() {
		return ErrClosed()
	}
	r.Status, r.ClosedReason = StatusClosed, reason
	r.ClosedAt, r.UpdatedAt = &now, now
	return nil
}

// Post applies a message by author to the request: it moves the status and
// the last activity time. It fails with a conflict when closed.
func (r *Request) Post(author AuthorType, now time.Time) error {
	next, err := StatusAfterPost(r.Status, author)
	if err != nil {
		return err
	}
	r.Status, r.LastActivityAt, r.UpdatedAt = next, now, now
	return nil
}

// NewRequest is what a requester submits.
type NewRequest struct {
	Topic        Topic
	ContactEmail string
	ContactName  string
	ContactPhone string
	OrderID      string
	PageURL      string
	Body         string
}

// Validate normalises the submission in place and reports every problem.
func (n *NewRequest) Validate() error {
	v := ycerrors.Validation()
	if !n.Topic.Valid() {
		v.Field("topic", "invalid", "Choose a topic from the list")
	}
	email, err := NormaliseEmail(n.ContactEmail)
	if err != nil {
		v.Field("contact_email", "invalid", "Enter a valid email address")
	}
	n.ContactEmail = email
	n.ContactName = strings.TrimSpace(n.ContactName)
	if n.ContactName == "" || utf8.RuneCountInString(n.ContactName) > MaxNameLength {
		v.Field("contact_name", "invalid", "Enter your name (up to 200 characters)")
	}
	if n.ContactPhone = strings.TrimSpace(n.ContactPhone); n.ContactPhone != "" && !phonePattern.MatchString(n.ContactPhone) {
		v.Field("contact_phone", "invalid", "Use international format, e.g. +27821234567")
	}
	if n.PageURL = strings.TrimSpace(n.PageURL); len(n.PageURL) > MaxPageURLLength {
		v.Field("page_url", "too_long", "Page URL is too long")
	}
	body, err := NormaliseBody(n.Body)
	if err != nil {
		v.Field("body", "invalid", BodyMessage(err))
	}
	n.Body = body
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// NormaliseEmail lowercases and trims an address and checks it is a bare
// addr-spec. The result is the join key for visitors and claims.
func NormaliseEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > MaxEmailLength {
		return "", errEmail
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", errEmail
	}
	if at := strings.LastIndexByte(s, '@'); at < 1 || !strings.Contains(s[at+1:], ".") {
		return "", errEmail
	}
	return s, nil
}

// NormaliseBody trims a message body and checks its length.
func NormaliseBody(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch n := utf8.RuneCountInString(s); {
	case n == 0:
		return "", ErrBodyEmpty
	case n > MaxBodyLength:
		return "", ErrBodyTooLong
	}
	return s, nil
}

// BodyMessage is the text shown to a requester for a NormaliseBody error.
func BodyMessage(err error) string {
	if errors.Is(err, ErrBodyTooLong) {
		return "Message is longer than 5000 characters"
	}
	return "Write a message"
}

// ValidateBody trims body and reports a validation error when it is empty
// or too long.
func ValidateBody(body string) (string, error) {
	body, err := NormaliseBody(body)
	if err != nil {
		return "", ycerrors.Validation().Field("body", "invalid", BodyMessage(err)).Build()
	}
	return body, nil
}

// Excerpt shortens body to at most MaxExcerpt characters for notifications.
func Excerpt(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) <= MaxExcerpt {
		return body
	}
	r := []rune(body)
	return strings.TrimSpace(string(r[:MaxExcerpt-1])) + "…"
}

// NewReference returns a reference such as SUP-7K3M9Q, drawn from r (use
// crypto/rand.Reader). It is unique only with high probability; the
// repository enforces uniqueness per shop.
func NewReference(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	n := len(referenceAlphabet)
	// Bytes at or above the largest multiple of n are discarded so every
	// character is equally likely.
	limit := byte(256 / n * n)
	var b strings.Builder
	b.WriteString(ReferencePrefix)
	one := make([]byte, 1)
	for count := 0; count < referenceLength; {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] >= limit {
			continue
		}
		b.WriteByte(referenceAlphabet[int(one[0])%n])
		count++
	}
	return b.String(), nil
}

// ValidReference reports whether s has the shape NewReference produces.
func ValidReference(s string) bool { return referencePattern.MatchString(s) }

// Errors from the Normalise functions.
var (
	errEmail       = errors.New("invalid email address")
	ErrBodyEmpty   = errors.New("message is empty")
	ErrBodyTooLong = errors.New("message is too long")
)
