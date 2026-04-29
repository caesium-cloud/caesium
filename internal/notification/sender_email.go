package notification

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// emailConfig is the expected JSON shape of an email channel's Config.
type emailConfig struct {
	SMTPHost string   `json:"smtp_host"`
	SMTPPort int      `json:"smtp_port"` // defaults to 587
	From     string   `json:"from"`
	To       []string `json:"to"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	// TLS controls STARTTLS behaviour: "starttls" (default), "tls", "none".
	TLS string `json:"tls,omitempty"`
}

// EmailSender sends notifications via SMTP email.
type EmailSender struct{}

// NewEmailSender creates an email notification sender.
func NewEmailSender() *EmailSender {
	return &EmailSender{}
}

func (s *EmailSender) Send(ctx context.Context, ch models.NotificationChannel, payload Payload) error {
	var cfg emailConfig
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("email: invalid channel config: %w", err)
	}

	if cfg.SMTPHost == "" {
		return fmt.Errorf("email: smtp_host is required in channel %q config", ch.Name)
	}
	if cfg.From == "" {
		return fmt.Errorf("email: from is required in channel %q config", ch.Name)
	}
	if len(cfg.To) == 0 {
		return fmt.Errorf("email: to is required in channel %q config", ch.Name)
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = 587
	}

	subject := formatEmailSubject(payload)
	body := formatEmailBody(payload)

	msg := buildMIMEMessage(cfg.From, cfg.To, subject, body)

	return sendMail(ctx, cfg, msg)
}

func formatEmailSubject(p Payload) string {
	prefix := "[Caesium]"
	name := friendlyEventName(p.EventType)
	if p.JobAlias != "" {
		return fmt.Sprintf("%s %s: %s", prefix, name, p.JobAlias)
	}
	return fmt.Sprintf("%s %s", prefix, name)
}

func formatEmailBody(p Payload) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Event: %s\n", friendlyEventName(p.EventType)))
	if p.JobAlias != "" {
		b.WriteString(fmt.Sprintf("Job: %s\n", p.JobAlias))
	}
	b.WriteString(fmt.Sprintf("Job ID: %s\n", p.JobID))
	if p.RunID.String() != "00000000-0000-0000-0000-000000000000" {
		b.WriteString(fmt.Sprintf("Run ID: %s\n", p.RunID))
	}
	if p.TaskID.String() != "00000000-0000-0000-0000-000000000000" {
		b.WriteString(fmt.Sprintf("Task ID: %s\n", p.TaskID))
	}
	b.WriteString(fmt.Sprintf("Timestamp: %s\n", p.Timestamp.Format(time.RFC3339)))
	if p.Error != "" {
		b.WriteString(fmt.Sprintf("\nError:\n%s\n", p.Error))
	}
	return b.String()
}

func buildMIMEMessage(from string, to []string, subject, body string) []byte {
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("From: %s\r\n", sanitizeHeader(from)))
	sanitizedTo := make([]string, len(to))
	for i, addr := range to {
		sanitizedTo[i] = sanitizeHeader(addr)
	}
	msg.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(sanitizedTo, ", ")))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject)))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)
	return []byte(msg.String())
}

// sanitizeHeader strips CR and LF characters from a header value to
// prevent SMTP header injection.
func sanitizeHeader(s string) string {
	r := strings.NewReplacer("\r", "", "\n", "")
	return r.Replace(s)
}

func sendMail(ctx context.Context, cfg emailConfig, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}

	tlsMode := strings.ToLower(strings.TrimSpace(cfg.TLS))
	if tlsMode == "" {
		tlsMode = "starttls"
	}

	var c *smtp.Client

	if tlsMode == "tls" {
		// Implicit TLS (port 465).
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.SMTPHost})
		c, err = smtp.NewClient(tlsConn, cfg.SMTPHost)
	} else {
		c, err = smtp.NewClient(conn, cfg.SMTPHost)
	}
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("email: smtp client: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if tlsMode == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.SMTPHost}); err != nil {
			return fmt.Errorf("email: starttls: %w", err)
		}
	}

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}

	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	for _, rcpt := range cfg.To {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("email: RCPT TO %s: %w", rcpt, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("email: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close body: %w", err)
	}

	return nil
}
