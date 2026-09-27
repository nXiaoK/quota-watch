package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualTelegramPrivateFlowWithRealStoreAndAdminClient(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var reads, resets, buttons, results atomic.Int32
	mainSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "manual-admin" {
			t.Error("missing admin key")
		}
		write := func(data any) { writeJSON(w, 200, map[string]any{"code": 0, "data": data}) }
		switch r.URL.Path {
		case "/api/v1/admin/accounts/1":
			index := reads.Add(1)
			percent := 40
			stamp := now.Add(-time.Minute)
			if index > 1 {
				percent = 0
				stamp = now
			}
			write(map[string]any{"id": 1, "name": "Local account", "platform": "openai", "type": "oauth", "credentials": map[string]any{"chatgpt_account_id": "stored-local", "plan_type": "plus"}, "extra": map[string]any{"codex_7d_used_percent": percent, "codex_7d_window_minutes": 10080, "codex_usage_updated_at": stamp.Format(time.RFC3339), "codex_7d_reset_at": now.Add(7 * 24 * time.Hour).Format(time.RFC3339)}})
		case "/api/v1/admin/subscriptions":
			if r.URL.Query().Get("user_id") != "8" && r.URL.Query().Get("group_id") != "9" {
				t.Error("manual validation queried unrelated subscription scope")
			}
			write(Page[Subscription]{Items: []Subscription{{ID: 10, UserID: 8, GroupID: 9, Status: "active", StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour)}}, Page: 1, PageSize: 1000, Total: 1, Pages: 1})
		case "/api/v1/admin/subscriptions/bulk-action":
			var payload subscriptionResetRequest
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Action != "reset_quota" || len(payload.SubscriptionIDs) != 1 || payload.SubscriptionIDs[0] != 10 || !payload.Weekly || payload.Daily || payload.Monthly || r.Header.Get("Idempotency-Key") == "" {
				t.Error("manual approval changed its selected scope")
			}
			resets.Add(1)
			write(ResetResult{SuccessCount: 1, Results: []ResetItem{{SubscriptionID: 10, Success: true}}})
		default:
			t.Errorf("forbidden upstream-triggering route %s", r.URL.Path)
			writeError(w, 404, "forbidden")
		}
	}))
	defer mainSite.Close()
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = mainSite.URL
	cfg.AdminAPIKey = "manual-admin"
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:manual-test", ChatID: "42"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.Rules = []Rule{{ID: "manual-rule", Name: "Manual rule", Enabled: true, AccountIDs: []int64{1}, SubscriptionIDs: []int64{10}, SubscriptionRefs: map[string]SubscriptionRef{"10": {UserID: 8, GroupID: 9}}, NotifyTelegram: true, Weekly: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store, nil)
	engine.wait = func(context.Context, time.Duration) error { return nil }
	engine.Notify = func(context.Context, Config, string, string) error { results.Add(1); return nil }
	tgClient := &http.Client{Transport: notificationRoundTripper(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/sendMessage") {
			var message telegramMessage
			if json.NewDecoder(request.Body).Decode(&message) != nil || message.ReplyMarkup == nil || len(message.ReplyMarkup.InlineKeyboard) != 2 {
				t.Fatal("approval buttons were missing")
			}
			buttons.Add(1)
		}
		return telegramTestResponse(200, `{"ok":true,"result":true}`), nil
	})}
	engine.NotifyInteractive = func(ctx context.Context, cfg Config, delivery Delivery) error {
		return sendInteractiveTelegramRequest(ctx, cfg.Telegram, delivery, tgClient)
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil || len(state.ManualRequests) != 1 || len(state.Actions) != 0 || resets.Load() != 0 || buttons.Load() != 1 {
		t.Fatal("global-off event reset before a person selected the button", err)
	}
	id := state.ManualRequests[0].ID
	receiver := NewTelegramReceiver(store, engine, nil)
	receiver.newHTTP = func(TelegramConfig, time.Duration) (*http.Client, func(), error) { return tgClient, func() {}, nil }
	if err := store.Update(func(state *State) error {
		state.TelegramReceiver.BotFingerprint = telegramBotFingerprint(cfg.Telegram.BotToken)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	callbackJSON := `{"update_id":101,"callback_query":{"id":"local-callback","from":{"id":42},"message":{"message_id":7,"chat":{"id":42,"type":"private"}},"data":"qw:r:` + id + `"}}`
	var update telegramUpdate
	if err := json.Unmarshal([]byte(callbackJSON), &update); err != nil {
		t.Fatal(err)
	}
	if err := receiver.handleUpdate(context.Background(), cfg, tgClient, telegramBotFingerprint(cfg.Telegram.BotToken), update); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Snapshot()
	if resets.Load() != 1 || state.ManualRequests[0].ApprovedBy != 42 || state.ManualRequests[0].Status != "succeeded" || len(state.Actions) != 1 || state.Actions[0].Mode != "manual" || results.Load() != 1 {
		t.Fatal("approved private callback did not reset only its selected subscription", state.ManualRequests, state.Actions)
	}
	if err := receiver.handleUpdate(context.Background(), cfg, tgClient, telegramBotFingerprint(cfg.Telegram.BotToken), update); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 1 {
		t.Fatal("repeated Telegram update reset the subscription again")
	}
	loaded, _ := store.Config()
	if loaded.AutoResetEnabled || state.Rules[0].AutoReset {
		t.Fatal("manual approval enabled an automatic switch")
	}
}
