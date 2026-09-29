package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	telegramResponseLimit = 64 << 10
	telegramUpdatesLimit  = 1 << 20
	telegramPollTimeout   = 40 * time.Second
)

type TelegramCallback struct {
	UpdateID     int64
	QueryID      string
	ChatID       int64
	ChatUsername string
	ChatType     string
	FromID       int64
	MessageID    int64
	Data         string
}

type ManualDecision struct {
	Text      string
	Accepted  bool
	RequestID string
}

type telegramButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

type telegramMarkup struct {
	InlineKeyboard [][]telegramButton `json:"inline_keyboard"`
}

type telegramMessage struct {
	ChatID                string          `json:"chat_id"`
	Text                  string          `json:"text"`
	DisableWebPagePreview bool            `json:"disable_web_page_preview"`
	ReplyMarkup           *telegramMarkup `json:"reply_markup,omitempty"`
}

func validTelegramToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	for _, ch := range parts[1] {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func newTelegramHTTPClient(cfg TelegramConfig, timeout time.Duration) (*http.Client, func(), error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.ProxyURL != "" {
		proxy, err := url.Parse(cfg.ProxyURL)
		if err != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
			return nil, nil, errors.New("Telegram proxy URL is invalid")
		}
		switch proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
			transport.Proxy = http.ProxyURL(proxy)
		default:
			return nil, nil, errors.New("Telegram proxy must use HTTP, HTTPS, or SOCKS5")
		}
	}
	return &http.Client{Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport.CloseIdleConnections, nil
}

func telegramRequest(ctx context.Context, cfg TelegramConfig, client *http.Client, method string, payload any, timeout time.Duration, limit int, result any) error {
	if !validTelegramToken(cfg.BotToken) {
		return errors.New("Telegram bot token is invalid")
	}
	content, err := json.Marshal(payload)
	if err != nil || len(content) > telegramResponseLimit {
		return errors.New("Telegram request payload could not be encoded or is too large")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+cfg.BotToken+"/"+method, strings.NewReader(string(content)))
	if err != nil {
		return errors.New("failed to create Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Telegram request failed; check connectivity and proxy settings")
	}
	defer resp.Body.Close()
	content, err = io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil || len(content) > limit {
		return errors.New("Telegram response could not be read or is too large")
	}
	var envelope struct {
		OK         *bool           `json:"ok"`
		Result     json.RawMessage `json:"result"`
		ErrorCode  int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.Unmarshal(content, &envelope)
	delay := time.Duration(min(max(envelope.Parameters.RetryAfter, 0), 86400)) * time.Second
	if headerDelay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); headerDelay > delay {
		delay = headerDelay
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, RetryAfter: delay, Message: fmt.Sprintf("Telegram request was rejected (HTTP %d)", resp.StatusCode)}
	}
	if decodeErr != nil || envelope.OK == nil {
		return errors.New("Telegram returned an invalid response")
	}
	if !*envelope.OK {
		return &APIError{Status: envelope.ErrorCode, RetryAfter: delay, Message: "Telegram did not accept the request; verify the bot and chat settings"}
	}
	if result != nil && (len(envelope.Result) == 0 || string(envelope.Result) == "null" || json.Unmarshal(envelope.Result, result) != nil) {
		return errors.New("Telegram returned an invalid result")
	}
	return nil
}

func sendTelegram(ctx context.Context, cfg TelegramConfig, message string) error {
	client, closeClient, err := newTelegramHTTPClient(cfg, notificationTimeout)
	if err != nil {
		return err
	}
	defer closeClient()
	return sendTelegramRequest(ctx, cfg, message, client)
}

func sendTelegramRequest(ctx context.Context, cfg TelegramConfig, message string, client *http.Client) error {
	return sendTelegramMessage(ctx, cfg, message, nil, client)
}

func sendTelegramMessage(ctx context.Context, cfg TelegramConfig, message string, markup *telegramMarkup, client *http.Client) error {
	if !validTelegramToken(cfg.BotToken) || strings.TrimSpace(cfg.ChatID) == "" || strings.ContainsAny(cfg.ChatID, "\r\n\x00") {
		return errors.New("Telegram bot token or chat ID is invalid")
	}
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		return errors.New("notification message must be nonempty UTF-8 text")
	}
	if utf8.RuneCountInString(message) > 4096 {
		if markup != nil {
			return errors.New("Telegram confirmation message exceeds 4096 characters; review and reset the selected subscriptions in the management page")
		}
		message = string([]rune(message)[:4095]) + "…"
	}
	return telegramRequest(ctx, cfg, client, "sendMessage", telegramMessage{cfg.ChatID, message, true, markup}, notificationTimeout, telegramResponseLimit, nil)
}

func SendInteractiveNotification(ctx context.Context, cfg Config, delivery Delivery) error {
	if delivery.Channel != "telegram" || delivery.ManualRequestID == "" {
		return SendNotification(ctx, cfg, delivery.Channel, delivery.Message)
	}
	if !cfg.Telegram.Enabled {
		return errors.New("Telegram notifications are disabled")
	}
	client, closeClient, err := newTelegramHTTPClient(cfg.Telegram, notificationTimeout)
	if err != nil {
		return err
	}
	defer closeClient()
	return sendInteractiveTelegramRequest(ctx, cfg.Telegram, delivery, client)
}

func sendInteractiveTelegramRequest(ctx context.Context, cfg TelegramConfig, delivery Delivery, client *http.Client) error {
	if !validManualRequestID(delivery.ManualRequestID) {
		return errors.New("manual reset request ID is invalid")
	}
	markup := &telegramMarkup{InlineKeyboard: [][]telegramButton{
		{{Text: "重置选定订阅", CallbackData: "qw:r:" + delivery.ManualRequestID}},
		{{Text: "忽略", CallbackData: "qw:i:" + delivery.ManualRequestID}},
	}}
	return sendTelegramMessage(ctx, cfg, delivery.Message, markup, client)
}

// sub2apiReleaseURL accepts only a release tag, never an arbitrary URL from
// the upstream version check. This keeps both the message and its link safe
// when the version information is malformed or unexpected.
func sub2apiReleaseURL(version string) (string, string, error) {
	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if len(tag) < 2 || len(tag) > 96 || tag[1] < '0' || tag[1] > '9' {
		return "", "", errors.New("Sub2API release version is invalid")
	}
	for _, ch := range tag[2:] {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '-' || ch == '_' || ch == '+') {
			return "", "", errors.New("Sub2API release version is invalid")
		}
	}
	return tag, "https://github.com/Wei-Shaw/sub2api/releases/tag/" + url.PathEscape(tag), nil
}

// SendUpdateVersionPrompt asks for a durable decision about one release. The
// callback carries only an opaque ID; the version remains in local state.
func SendUpdateVersionPrompt(ctx context.Context, cfg Config, version, requestID string) error {
	if !cfg.Telegram.Enabled {
		return errors.New("Telegram notifications are disabled")
	}
	client, closeClient, err := newTelegramHTTPClient(cfg.Telegram, notificationTimeout)
	if err != nil {
		return err
	}
	defer closeClient()
	return sendUpdateVersionPromptRequest(ctx, cfg.Telegram, version, requestID, client)
}

func sendUpdateVersionPromptRequest(ctx context.Context, cfg TelegramConfig, version, requestID string, client *http.Client) error {
	if !validManualRequestID(requestID) {
		return errors.New("update approval request ID is invalid")
	}
	tag, releaseURL, err := sub2apiReleaseURL(version)
	if err != nil {
		return err
	}
	markup := &telegramMarkup{InlineKeyboard: [][]telegramButton{
		{{Text: "查看版本说明", URL: releaseURL}},
		{{Text: "加入空闲更新队列", CallbackData: "qw:ua:" + requestID}},
		{{Text: "拒绝此版本", CallbackData: "qw:ur:" + requestID}},
	}}
	message := "检测到 Sub2API 新版本：" + tag + "\n版本链接：" + releaseURL + "\n是否将此版本加入空闲更新队列？"
	return sendTelegramMessage(ctx, cfg, message, markup, client)
}

func validManualRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}
