// Package email handles transactional emails for booking notifications.
//
// Design constraints (1GB DigitalOcean droplet, see CLAUDE.md):
//   - No buffered in-memory queues; sends are synchronous from the caller.
//   - Every Send has a context deadline so a stuck SMTP server can't pin the
//     booking handler indefinitely.
//   - When SMTP is not configured the Mailer falls back to LogMailer so dev
//     environments never fail to create a booking just because email isn't
//     set up.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Mailer is the surface used by handlers. Implementations may persist, log,
// or send via SMTP; the contract is just "best-effort deliver this message".
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Message is the wire-agnostic payload. Attachments use the `Attachment`
// type so the ICS calendar invite is just one more entry rather than a
// special-case in the renderer.
type Message struct {
	To          []string
	From        string
	Subject     string
	HTMLBody    string
	TextBody    string
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	Body        []byte
}

// Config is what Load() emits — the caller decides which concrete Mailer to
// build from it (typically `NewFromConfig`).
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	// SendTimeout caps each Send() call. Without this a hung SMTP handshake
	// would block the booking handler past any sane request timeout.
	SendTimeout time.Duration
}

// LoadConfig pulls SMTP settings from environment variables. Returns
// `enabled=false` when SMTP_HOST is absent so dev defaults to a log-only
// path rather than failing to boot.
func LoadConfig() (Config, bool) {
	host := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	if host == "" {
		return Config{}, false
	}
	port := atoiOr(os.Getenv("SMTP_PORT"), 587)
	from := strings.TrimSpace(os.Getenv("EMAIL_FROM"))
	if from == "" {
		from = "Sessionly <no-reply@" + host + ">"
	}
	return Config{
		Host:        host,
		Port:        port,
		User:        os.Getenv("SMTP_USER"),
		Password:    os.Getenv("SMTP_PASSWORD"),
		From:        from,
		SendTimeout: 8 * time.Second,
	}, true
}

// NewFromEnv returns an SMTPMailer if SMTP_HOST is set, otherwise a
// LogMailer. Either way the caller gets a working Mailer.
func NewFromEnv() Mailer {
	cfg, ok := LoadConfig()
	if !ok {
		slog.Info("email: SMTP_HOST not set, using log-only mailer")
		return &LogMailer{}
	}
	slog.Info("email: SMTP mailer enabled", "host", cfg.Host, "port", cfg.Port, "from", cfg.From)
	return &SMTPMailer{cfg: cfg}
}

// LogMailer prints every message to slog at info level. Used in dev so the
// developer can copy/paste the meet code from the log instead of waiting on
// real email delivery.
type LogMailer struct{}

func (l *LogMailer) Send(_ context.Context, msg Message) error {
	slog.Info("email: log-only send",
		"to", msg.To, "from", msg.From, "subject", msg.Subject,
		"text_len", len(msg.TextBody), "attachments", len(msg.Attachments))
	return nil
}

// SMTPMailer sends via plain SMTP with STARTTLS. The timeout in cfg.SendTimeout
// covers the full Send() call — handshake + AUTH + DATA — so a slow or hung
// server can never block longer than that.
type SMTPMailer struct {
	cfg Config
}

func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	if len(msg.To) == 0 {
		return errors.New("email: To is empty")
	}
	deadline := time.Now().Add(m.cfg.SendTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	dialer := &net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	// STARTTLS is mandatory on most providers (Gmail, Mailgun, SES). If the
	// server doesn't advertise it we skip — useful only for trusted local
	// dev relays.
	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: m.cfg.Host}
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if m.cfg.User != "" {
		auth := smtp.PlainAuth("", m.cfg.User, m.cfg.Password, m.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	from := msg.From
	if from == "" {
		from = m.cfg.From
	}
	if err := client.Mail(addressOnly(from)); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	for _, to := range msg.To {
		if err := client.Rcpt(addressOnly(to)); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	body, err := encodeMessage(msg, from)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp body close: %w", err)
	}
	return client.Quit()
}

// addressOnly strips a display-name wrapper from an address line. Net/smtp's
// Mail/Rcpt expect a bare addr-spec, not "Name <foo@bar>".
func addressOnly(s string) string {
	if addr, err := mail.ParseAddress(sanitizeHeaderValue(s)); err == nil {
		return addr.Address
	}
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.LastIndex(s, ">"); j > i {
			return sanitizeHeaderValue(s[i+1 : j])
		}
	}
	return sanitizeHeaderValue(s)
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
