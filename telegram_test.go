package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func telegramTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestInteractiveTelegramMessageContainsOnlyOpaqueCallbackIDs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	client := &http.Client{Transport: notificationRoundTripper(func(req *http.Request) (*http.Response, error) {
		var payload telegramMessage
		if req.URL.Path != "/bot123:secret-token/sendMessage" || json.NewDecoder(req.Body).Decode(&payload) != nil {
			t.Fatal("invalid interactive Telegram request")
		}
		if payload.ChatID != "-100123" || payload.Text != "待确认：订阅 #42；周用量" || !payload.DisableWebPagePreview {
			t.Fatalf("interactive notification changed message scope: %#v", payload)
		}
		if payload.ReplyMarkup == nil || len(payload.ReplyMarkup.InlineKeyboard) != 2 {
			t.Fatal("manual approval buttons are missing")
		}
		for i, prefix := range []string{"qw:r:", "qw:i:"} {
			buttons := payload.ReplyMarkup.InlineKeyboard[i]
			if len(buttons) != 1 || buttons[0].CallbackData != prefix+id || len(buttons[0].CallbackData) > 64 {
				t.Fatalf("callback exposed targets or was malformed: %#v", buttons)
			}
		}
		return telegramTestResponse(200, `{"ok":true,"result":{"message_id":1}}`), nil
	})}
	err := sendInteractiveTelegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token", ChatID: "-100123"}, Delivery{ManualRequestID: id, Message: "待确认：订阅 #42；周用量"}, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"42", id + "x", strings.ToUpper(id), "../../" + id} {
		err := sendInteractiveTelegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token", ChatID: "-100123"}, Delivery{ManualRequestID: invalid, Message: "test"}, client)
		if err == nil {
			t.Fatalf("invalid callback ID accepted: %q", invalid)
		}
	}
	if err := sendInteractiveTelegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token", ChatID: "-100123"}, Delivery{ManualRequestID: id, Message: strings.Repeat("订", 4097)}, client); err == nil {
		t.Fatal("interactive confirmation silently hid some selected targets")
	}
}

func TestUpdateVersionPromptLinksToReleaseAndUsesOpaqueCallbackIDs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	const releaseURL = "https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.10"
	client := &http.Client{Transport: notificationRoundTripper(func(req *http.Request) (*http.Response, error) {
		var payload telegramMessage
		if req.URL.Path != "/bot123:secret-token/sendMessage" || json.NewDecoder(req.Body).Decode(&payload) != nil {
			t.Fatal("invalid update version Telegram request")
		}
		if payload.ChatID != "42" || !payload.DisableWebPagePreview || !strings.Contains(payload.Text, "v0.2.10") || !strings.Contains(payload.Text, releaseURL) {
			t.Fatalf("update notification omitted the release link: %#v", payload)
		}
		if payload.ReplyMarkup == nil || len(payload.ReplyMarkup.InlineKeyboard) != 3 {
			t.Fatal("update version buttons are missing")
		}
		buttons := payload.ReplyMarkup.InlineKeyboard
		if len(buttons[0]) != 1 || buttons[0][0].URL != releaseURL || buttons[0][0].CallbackData != "" {
			t.Fatal("release button does not point to the fixed upstream repository")
		}
		for i, prefix := range []string{"qw:ua:", "qw:ur:"} {
			button := buttons[i+1][0]
			if button.CallbackData != prefix+id || len(button.CallbackData) > 64 || button.URL != "" || strings.Contains(button.CallbackData, "0.2.10") {
				t.Fatalf("update callback exposed version or was malformed: %#v", button)
			}
		}
		return telegramTestResponse(200, `{"ok":true,"result":{"message_id":1}}`), nil
	})}
	cfg := TelegramConfig{BotToken: "123:secret-token", ChatID: "42"}
	for _, version := range []string{"0.2.10", "v0.2.10"} {
		if err := sendUpdateVersionPromptRequest(context.Background(), cfg, version, id, client); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{"v0.2.10\nhttps://evil.example", "../../evil", "v0.2.10?redirect=evil", "v0.2.10/evil", " v0.2.10"} {
		if err := sendUpdateVersionPromptRequest(context.Background(), cfg, invalid, id, client); err == nil {
			t.Fatalf("unsafe release version accepted: %q", invalid)
		}
	}
	if err := sendUpdateVersionPromptRequest(context.Background(), cfg, "v0.2.10", "42", client); err == nil {
		t.Fatal("nonopaque update request ID accepted")
	}
}

func TestTelegramHTTPDefaultsAndProxyValidation(t *testing.T) {
	client, closeClient, err := newTelegramHTTPClient(TelegramConfig{}, telegramPollTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || client.Timeout != telegramPollTimeout {
		t.Fatal("Telegram TLS, proxy, or timeout defaults changed")
	}
	if client.CheckRedirect(&http.Request{}, nil) != http.ErrUseLastResponse {
		t.Fatal("Telegram redirects must not forward bot credentials")
	}
	for _, proxy := range []string{"https://proxy-user:proxy-password@%", "ftp://proxy-user:proxy-password@host", "http://host/path", "http://host?password=proxy-password"} {
		_, _, err := newTelegramHTTPClient(TelegramConfig{ProxyURL: proxy}, notificationTimeout)
		if err == nil || strings.Contains(err.Error(), "proxy-password") || strings.Contains(err.Error(), "proxy-user") {
			t.Fatalf("invalid proxy accepted or disclosed: %v", err)
		}
	}
	for _, proxy := range []string{"http://127.0.0.1:7890", "https://proxy-user:proxy-password@127.0.0.1:7890", "socks5://127.0.0.1:1080", "socks5h://127.0.0.1:1080"} {
		_, closeClient, err := newTelegramHTTPClient(TelegramConfig{ProxyURL: proxy}, notificationTimeout)
		if err != nil {
			t.Fatal(err)
		}
		closeClient()
	}
}

func TestTelegramTransportResponseLimitsAndSanitizedErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"oversized", 200, strings.Repeat("secret-token", telegramResponseLimit)},
		{"invalid", 200, "secret-token proxy-password https://private-proxy"},
		{"rejected", 200, `{"ok":false,"error_code":403,"description":"secret-token proxy-password"}`},
		{"redirect", 302, `{"ok":false,"description":"secret-token proxy-password"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: notificationRoundTripper(func(req *http.Request) (*http.Response, error) {
				return telegramTestResponse(test.status, test.body), nil
			})}
			err := telegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token"}, client, "getUpdates", map[string]int{"timeout": 30}, notificationTimeout, telegramResponseLimit, nil)
			if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "proxy-password") || strings.Contains(err.Error(), "https://private-proxy") {
				t.Fatalf("Telegram error not safely bounded: %v", err)
			}
		})
	}
	client := &http.Client{Transport: notificationRoundTripper(func(req *http.Request) (*http.Response, error) {
		resp := telegramTestResponse(429, `{"ok":false,"parameters":{"retry_after":999999},"description":"private"}`)
		resp.Header.Set("Retry-After", "999999")
		return resp, nil
	})}
	err := telegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token"}, client, "getUpdates", map[string]int{}, notificationTimeout, telegramResponseLimit, nil)
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.RetryAfter != 24*time.Hour {
		t.Fatalf("Telegram retry delay was not bounded: %v", err)
	}
	client.Transport = notificationRoundTripper(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("request failed %s proxy-password", req.URL)
	})
	err = telegramRequest(context.Background(), TelegramConfig{BotToken: "123:secret-token"}, client, "getUpdates", nil, notificationTimeout, telegramResponseLimit, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "proxy-password") {
		t.Fatalf("Telegram transport leaked credentials: %v", err)
	}
}
