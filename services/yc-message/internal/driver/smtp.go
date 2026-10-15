package driver

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// SMTPConfig is the `smtp:` block.
type SMTPConfig struct {
	// Addr is host:port; empty turns email off.
	Addr     string `koanf:"addr"`
	From     string `koanf:"from"`
	FromName string `koanf:"from_name"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	// StartTLS requires the connection to be upgraded before sending.
	// MailHog does not offer it.
	StartTLS bool          `koanf:"starttls"`
	Timeout  time.Duration `koanf:"timeout"`
}

// SMTP sends email through an SMTP relay.
type SMTP struct {
	cfg SMTPConfig
}

// NewSMTP returns an SMTP driver.
func NewSMTP(cfg SMTPConfig) *SMTP {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &SMTP{cfg: cfg}
}

// Name is "smtp".
func (*SMTP) Name() string { return "smtp" }

// Channel is email.
func (*SMTP) Channel() domain.Channel { return domain.ChannelEmail }

// Send delivers m and returns its Message-ID. A 5xx reply is permanent.
func (s *SMTP) Send(ctx context.Context, m Outgoing) (string, error) {
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return "", fmt.Errorf("%w: recipient %q: %w", ErrPermanent, m.To, err)
	}
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return "", fmt.Errorf("smtp addr %q: %w", s.cfg.Addr, err)
	}
	id := fmt.Sprintf("<%s@%s>", uuid.NewString(), domainOf(s.cfg.From))
	msg, err := s.compose(id, to, m)
	if err != nil {
		return "", err
	}
	if err := s.deliver(ctx, host, to.Address, msg); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) && reply.Code >= 500 {
			return "", fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		return "", err
	}
	return id, nil
}

func (s *SMTP) deliver(ctx context.Context, host, to string, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if s.cfg.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

var headerSafe = strings.NewReplacer("\r", " ", "\n", " ")

func (s *SMTP) compose(id string, to *mail.Address, m Outgoing) ([]byte, error) {
	from := mail.Address{Name: s.cfg.FromName, Address: s.cfg.From}
	contentType := "text/plain; charset=utf-8"
	if m.Format == domain.FormatHTML {
		contentType = "text/html; charset=utf-8"
	}
	var b bytes.Buffer
	for _, h := range [][2]string{
		{"From", from.String()},
		{"To", to.String()},
		{"Subject", mime.QEncoding.Encode("utf-8", headerSafe.Replace(m.Subject))},
		{"Date", time.Now().UTC().Format(time.RFC1123Z)},
		{"Message-ID", id},
		{"MIME-Version", "1.0"},
		{"Content-Type", contentType},
		{"Content-Transfer-Encoding", "quoted-printable"},
	} {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func domainOf(addr string) string {
	if _, d, ok := strings.Cut(addr, "@"); ok && d != "" {
		return d
	}
	return "yc.local"
}
