package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type notificationRoundTripper func(*http.Request) (*http.Response, error)

func (f notificationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTelegramUsesJSONAndDoesNotExposeTokenOnFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("invalid Telegram request")
				}
				if payload["chat_id"] != "-100123" || payload["text"] != "中文额度已归零\n正常正文" {
					t.Errorf("Telegram JSON lost text: %#v", payload)
				}
				if _, exists := payload["parse_mode"]; exists {
					t.Error("message should remain plain text")
				}
				w.WriteHeader(status)
				if status == http.StatusOK {
					fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
				} else {
					fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"secret-token password","parameters":{"retry_after":17}}`)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			client := &http.Client{Transport: notificationRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.telegram.org" || r.URL.Path != "/bot123:secret-token/sendMessage" {
					t.Errorf("unexpected Telegram endpoint")
				}
				copy := r.Clone(r.Context())
				endpoint := *r.URL
				endpoint.Scheme = target.Scheme
				endpoint.Host = target.Host
				copy.URL = &endpoint
				return http.DefaultTransport.RoundTrip(copy)
			})}
			err := sendTelegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token", ChatID: "-100123"}, "中文额度已归零\n正常正文", client)
			if status == http.StatusOK {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.RetryAfter != 17*time.Second {
				t.Fatalf("Telegram retry: %v", err)
			}
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("Telegram failure exposed credentials")
			}
		})
	}
	client := &http.Client{Transport: notificationRoundTripper(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("request failed: %s proxy-password", r.URL.String())
	})}
	err := sendTelegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token", ChatID: "123"}, "test", client)
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "proxy-password") {
		t.Fatalf("transport failure leaked secrets: %v", err)
	}
}

func TestNotificationValidationDoesNotSendWithInjectedHeaders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email = EmailConfig{Enabled: true, Host: "127.0.0.1", Port: 587, TLSMode: "starttls", From: "sender@example.com\r\nBcc: extra@example.com", Recipients: []string{"receiver@example.com"}}
	if err := SendNotification(context.Background(), cfg, "email", "test"); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("sender header injection accepted: %v", err)
	}
	cfg.Email.From = "sender@example.com"
	cfg.Email.Recipients = []string{"receiver@example.com\nBcc: extra@example.com"}
	if err := SendNotification(context.Background(), cfg, "email", "test"); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("recipient header injection accepted: %v", err)
	}
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:token/redirect", ChatID: "12"}
	if err := SendNotification(context.Background(), cfg, "telegram", "test"); err == nil {
		t.Fatal("Telegram endpoint token injection accepted")
	}
	cfg.Telegram.BotToken = "123:valid_token"
	cfg.Telegram.ProxyURL = "https://proxy-user:proxy-password@%"
	if err := SendNotification(context.Background(), cfg, "telegram", "test"); err == nil || strings.Contains(err.Error(), "proxy-password") {
		t.Fatalf("proxy URL error leaked credentials: %v", err)
	}
}

type smtpObservation struct {
	SecureAuth bool
	Auth       string
	Data       string
	Error      error
}

func localSMTPServer(t *testing.T, mode string, offerTLS, rejectAuth bool) (EmailConfig, *tls.Config, <-chan smtpObservation) {
	t.Helper()
	certificateSource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverTLS := &tls.Config{Certificates: certificateSource.TLS.Certificates, MinVersion: tls.VersionTLS12}
	roots := x509.NewCertPool()
	roots.AddCert(certificateSource.Certificate())
	certificateSource.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	observations := make(chan smtpObservation, 1)
	go func() {
		observed := smtpObservation{}
		defer func() { observations <- observed }()
		conn, err := listener.Accept()
		if err != nil {
			observed.Error = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		secure := false
		if mode == "tls" {
			tlsConn := tls.Server(conn, serverTLS)
			if err := tlsConn.Handshake(); err != nil {
				observed.Error = err
				return
			}
			conn = tlsConn
			secure = true
		}
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		respond := func(text string) error {
			if _, err := writer.WriteString(text + "\r\n"); err != nil {
				return err
			}
			return writer.Flush()
		}
		if err := respond("220 local SMTP ready"); err != nil {
			observed.Error = err
			return
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					observed.Error = err
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO "):
				response := "250-local SMTP\r\n"
				if offerTLS && !secure {
					response += "250-STARTTLS\r\n"
				}
				response += "250 AUTH PLAIN"
				if err := respond(response); err != nil {
					observed.Error = err
					return
				}
			case line == "STARTTLS":
				if err := respond("220 begin TLS"); err != nil {
					observed.Error = err
					return
				}
				tlsConn := tls.Server(conn, serverTLS)
				if err := tlsConn.Handshake(); err != nil {
					observed.Error = err
					return
				}
				conn = tlsConn
				secure = true
				reader = bufio.NewReader(conn)
				writer = bufio.NewWriter(conn)
			case strings.HasPrefix(line, "AUTH PLAIN "):
				observed.SecureAuth = secure
				decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				observed.Auth = string(decoded)
				if rejectAuth {
					_ = respond("535 SMTP-private password-private authentication rejected")
				} else {
					_ = respond("235 authenticated")
				}
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				_ = respond("250 accepted")
			case line == "DATA":
				if err := respond("354 send email"); err != nil {
					observed.Error = err
					return
				}
				var data strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						observed.Error = err
						return
					}
					if line == ".\r\n" {
						break
					}
					data.WriteString(strings.TrimPrefix(line, "."))
				}
				observed.Data = data.String()
				_ = respond("250 queued")
			case line == "QUIT":
				_ = respond("221 goodbye")
				return
			default:
				observed.Error = fmt.Errorf("unexpected SMTP command")
				return
			}
		}
	}()
	cfg := EmailConfig{Enabled: true, Host: host, Port: port, TLSMode: mode, Username: "smtp-user", Password: "password-private", From: "发件人 <sender@example.com>", Recipients: []string{"收件人 <receiver@example.com>"}}
	return cfg, &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}, observations
}

func TestSMTPVerifiedTLSAndUTF8MIME(t *testing.T) {
	for _, mode := range []string{"starttls", "tls"} {
		t.Run(mode, func(t *testing.T) {
			cfg, tlsConfig, observations := localSMTPServer(t, mode, true, false)
			message := "账号 7d 从 45% 归零\n正文里的换行\r\nBcc: 这仍然是正文"
			if err := sendEmailWithTLSConfig(context.Background(), cfg, message, tlsConfig); err != nil {
				t.Fatal(err)
			}
			observed := <-observations
			if observed.Error != nil || !observed.SecureAuth || observed.Auth != "\x00smtp-user\x00password-private" {
				t.Fatalf("SMTP authentication was not protected: %#v", observed)
			}
			parsed, err := mail.ReadMessage(strings.NewReader(observed.Data))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Header.Get("Bcc") != "" || parsed.Header.Get("Content-Transfer-Encoding") != "base64" {
				t.Fatal("email header or encoding was unsafe")
			}
			decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, parsed.Body))
			if err != nil || string(decoded) != message {
				t.Fatalf("email body changed: %q %v", decoded, err)
			}
		})
	}
}

func TestSMTPRequiresTLSBeforeSendingCredentials(t *testing.T) {
	cfg, tlsConfig, observations := localSMTPServer(t, "starttls", false, false)
	err := sendEmailWithTLSConfig(context.Background(), cfg, "test", tlsConfig)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("missing TLS was accepted: %v", err)
	}
	observed := <-observations
	if observed.Auth != "" || observed.Data != "" {
		t.Fatal("SMTP credentials or email data were sent without TLS")
	}
}

func TestSMTPFailureTextDoesNotExposePassword(t *testing.T) {
	cfg, tlsConfig, observations := localSMTPServer(t, "tls", true, true)
	err := sendEmailWithTLSConfig(context.Background(), cfg, "test", tlsConfig)
	if err == nil || strings.Contains(err.Error(), "password-private") || strings.Contains(err.Error(), "SMTP-private") {
		t.Fatalf("SMTP rejection exposed credentials: %v", err)
	}
	<-observations
	cfg, _, observations = localSMTPServer(t, "tls", true, false)
	err = sendEmail(context.Background(), cfg, "test")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate accepted: %v", err)
	}
	<-observations
}
