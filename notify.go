package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const notificationTimeout = 30 * time.Second

const (
	quotaEmailSubject  = "Sub2API 7d 额度通知"
	updateEmailSubject = "Sub2API 自动更新成功"
)

func SendNotification(ctx context.Context, cfg Config, channel, message string) error {
	return sendNotification(ctx, cfg, channel, quotaEmailSubject, message)
}

func SendUpdateNotification(ctx context.Context, cfg Config, channel, message string) error {
	return sendNotification(ctx, cfg, channel, updateEmailSubject, message)
}

func sendNotification(ctx context.Context, cfg Config, channel, subject, message string) error {
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		return errors.New("notification message must be nonempty UTF-8 text")
	}
	switch channel {
	case "telegram":
		if !cfg.Telegram.Enabled {
			return errors.New("Telegram notifications are disabled")
		}
		return sendTelegram(ctx, cfg.Telegram, message)
	case "email":
		if !cfg.Email.Enabled {
			return errors.New("email notifications are disabled")
		}
		return sendEmailWithSubject(ctx, cfg.Email, subject, message)
	default:
		return errors.New("unsupported notification channel")
	}
}

func sendEmail(ctx context.Context, cfg EmailConfig, message string) error {
	return sendEmailWithTLSConfig(ctx, cfg, message, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
}

func sendEmailWithSubject(ctx context.Context, cfg EmailConfig, subject, message string) error {
	return sendEmailWithTLSConfigAndSubject(ctx, cfg, subject, message, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
}

func emailAddresses(cfg EmailConfig) (*mail.Address, []*mail.Address, error) {
	if strings.ContainsAny(cfg.From, "\r\n\x00") {
		return nil, nil, errors.New("email sender contains invalid header characters")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, nil, errors.New("email sender address is invalid")
	}
	if len(cfg.Recipients) == 0 {
		return nil, nil, errors.New("at least one email recipient is required")
	}
	recipients := make([]*mail.Address, 0, len(cfg.Recipients))
	seen := make(map[string]bool)
	for _, raw := range cfg.Recipients {
		if strings.ContainsAny(raw, "\r\n\x00") {
			return nil, nil, errors.New("email recipient contains invalid header characters")
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, nil, errors.New("email recipient address is invalid")
		}
		if !seen[addr.Address] {
			recipients = append(recipients, addr)
			seen[addr.Address] = true
		}
	}
	return from, recipients, nil
}

func emailContent(from *mail.Address, recipients []*mail.Address, subject, message string) string {
	to := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		to = append(to, recipient.String())
	}
	var content strings.Builder
	content.WriteString("From: " + from.String() + "\r\n")
	content.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	content.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	content.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	content.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(message))
	for len(encoded) > 76 {
		content.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	content.WriteString(encoded + "\r\n")
	return content.String()
}

func sendEmailWithTLSConfig(ctx context.Context, cfg EmailConfig, message string, tlsConfig *tls.Config) error {
	return sendEmailWithTLSConfigAndSubject(ctx, cfg, quotaEmailSubject, message, tlsConfig)
}

func sendEmailWithTLSConfigAndSubject(ctx context.Context, cfg EmailConfig, subject, message string, tlsConfig *tls.Config) error {
	if !utf8.ValidString(subject) || strings.TrimSpace(subject) == "" || strings.ContainsAny(subject, "\r\n\x00") {
		return errors.New("email subject is invalid")
	}
	if cfg.Host == "" || strings.TrimSpace(cfg.Host) != cfg.Host || strings.ContainsAny(cfg.Host, "\r\n\x00 /\\@") || cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("SMTP host or port is invalid")
	}
	if cfg.TLSMode != "starttls" && cfg.TLSMode != "tls" {
		return errors.New("SMTP TLS mode must be starttls or tls")
	}
	if strings.ContainsAny(cfg.Username, "\r\n\x00") || (cfg.Username == "" && cfg.Password != "") {
		return errors.New("SMTP authentication settings are invalid")
	}
	from, recipients, err := emailAddresses(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return errors.New("SMTP connection failed; check the server and port")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if conn.SetDeadline(deadline) != nil {
			return errors.New("SMTP connection deadline could not be set")
		}
	}
	rawConn := conn
	stopCancel := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stopCancel()
	if cfg.TLSMode == "tls" {
		tlsConn := tls.Client(conn, tlsConfig)
		if tlsConn.HandshakeContext(ctx) != nil {
			return errors.New("SMTP TLS handshake failed; verify the server certificate and TLS mode")
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return errors.New("SMTP greeting failed")
	}
	defer client.Close()
	if cfg.TLSMode == "starttls" {
		if advertised, _ := client.Extension("STARTTLS"); !advertised {
			return errors.New("SMTP server does not support required STARTTLS")
		}
		if client.StartTLS(tlsConfig) != nil {
			return errors.New("SMTP STARTTLS failed; verify the server certificate")
		}
	}
	if cfg.Username != "" {
		if client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)) != nil {
			return errors.New("SMTP authentication failed; verify the username and password")
		}
	}
	if client.Mail(from.Address) != nil {
		return errors.New("SMTP server rejected the configured sender")
	}
	for _, recipient := range recipients {
		if client.Rcpt(recipient.Address) != nil {
			return errors.New("SMTP server rejected an email recipient")
		}
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("SMTP server did not accept email data")
	}
	if _, err := io.WriteString(writer, emailContent(from, recipients, subject, message)); err != nil {
		return errors.New("SMTP email transmission failed")
	}
	if writer.Close() != nil {
		return errors.New("SMTP email acceptance could not be confirmed")
	}
	_ = client.Quit()
	return nil
}
