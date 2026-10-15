package domain

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Format is how a template's body is written and sent.
type Format string

// Body formats. SMS bodies are always text.
const (
	FormatText Format = "text"
	FormatHTML Format = "html"
)

// Template limits.
const (
	MaxSubjectLen = 200
	MaxBodyLen    = 20_000
	MaxSMSLen     = 480 // three SMS segments
)

// Template is a message type's wording for one channel and locale.
type Template struct {
	TenantID string
	Type     Type
	Channel  Channel
	Locale   string
	Subject  string // email only
	Body     string
	Format   Format
	// Platform is true for a built-in default, false for a shop's own.
	Platform  bool
	Version   int
	UpdatedAt time.Time
}

// Rendered is a template applied to a context.
type Rendered struct {
	Subject string
	Body    string
	Format  Format
}

// Render applies the template to ctx. A missing context key is an error,
// so a typo in a template fails loudly instead of sending "<no value>".
func (t Template) Render(ctx map[string]any) (Rendered, error) {
	subject, err := renderText("subject", t.Subject, ctx)
	if err != nil {
		return Rendered{}, err
	}
	var body string
	if t.Format == FormatHTML {
		body, err = renderHTML(t.Body, ctx)
	} else {
		body, err = renderText("body", t.Body, ctx)
	}
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{Subject: strings.TrimSpace(subject), Body: body, Format: t.Format}, nil
}

// Validate checks a shop's template: its fields, and that it renders
// against the message type's sample context.
func (t Template) Validate() error {
	v := ycerrors.Validation()
	if !t.Type.Valid() {
		v.Field("type", "invalid", "Unknown message type")
	}
	if t.Channel != ChannelEmail && t.Channel != ChannelSMS {
		v.Field("channel", "invalid", "Templates are for email or sms")
	}
	if !localeRE.MatchString(t.Locale) {
		v.Field("locale", "invalid", "Locale is a BCP 47 tag such as en or en-ZA")
	}
	switch {
	case t.Channel == ChannelEmail && strings.TrimSpace(t.Subject) == "":
		v.Field("subject", "required", "An email needs a subject")
	case len(t.Subject) > MaxSubjectLen:
		v.Field("subject", "invalid", fmt.Sprintf("Subject is at most %d characters", MaxSubjectLen))
	case t.Channel == ChannelSMS && t.Subject != "":
		v.Field("subject", "invalid", "An SMS has no subject")
	}
	switch {
	case strings.TrimSpace(t.Body) == "":
		v.Field("body", "required", "Body is required")
	case len(t.Body) > MaxBodyLen:
		v.Field("body", "invalid", fmt.Sprintf("Body is at most %d characters", MaxBodyLen))
	case t.Channel == ChannelSMS && len(t.Body) > MaxSMSLen:
		v.Field("body", "invalid", fmt.Sprintf("An SMS template is at most %d characters", MaxSMSLen))
	}
	if t.Format != FormatText && t.Format != FormatHTML {
		v.Field("format", "invalid", "Format is text or html")
	} else if t.Channel == ChannelSMS && t.Format != FormatText {
		v.Field("format", "invalid", "An SMS is text")
	}
	if v.HasErrors() {
		return v.Build()
	}
	sample := Sample(t.Type)
	if _, err := renderText("subject", t.Subject, sample); err != nil {
		return ycerrors.Validation().Field("subject", "invalid", templateError(err)).Build()
	}
	var err error
	if t.Format == FormatHTML {
		_, err = renderHTML(t.Body, sample)
	} else {
		_, err = renderText("body", t.Body, sample)
	}
	if err != nil {
		return ycerrors.Validation().Field("body", "invalid", templateError(err)).Build()
	}
	return nil
}

func templateError(err error) string {
	return "Template does not render with this message type's fields: " + err.Error()
}

func renderText(name, src string, ctx map[string]any) (string, error) {
	t, err := texttemplate.New(name).Option("missingkey=error").Parse(src)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, ctx); err != nil {
		return "", err
	}
	return b.String(), nil
}

func renderHTML(src string, ctx map[string]any) (string, error) {
	t, err := htmltemplate.New("body").Option("missingkey=error").Parse(src)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, ctx); err != nil {
		return "", err
	}
	return b.String(), nil
}
