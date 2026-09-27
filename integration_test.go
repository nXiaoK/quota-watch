package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRealAdminClientAndPersistentEngineEndToEnd(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var reads, resets, notices, forbidden atomic.Int32
	mainSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "integration-admin-key" {
			t.Error("missing admin auth")
		}
		write := func(value any) { writeJSON(w, 200, map[string]any{"code": 0, "data": value}) }
		switch r.URL.Path {
		case "/api/v1/admin/accounts/1":
			index := reads.Add(1) - 1
			percent := float64(0)
			sampledAt := now.Add(-2 * time.Second)
			if index == 0 {
				percent = 63
				sampledAt = now.Add(-3 * time.Second)
			}
			write(map[string]any{"id": 1, "name": "Account A", "platform": "openai", "type": "oauth", "status": "active",
				"credentials": map[string]any{"chatgpt_account_id": "stored-one", "plan_type": "plus"},
				"extra": map[string]any{"codex_7d_used_percent": percent, "codex_7d_window_minutes": 10080,
					"codex_7d_reset_at": now.Add(7 * 24 * time.Hour).Format(time.RFC3339), "codex_usage_updated_at": sampledAt.Format(time.RFC3339)}})
		case "/api/v1/admin/subscriptions":
			write(Page[Subscription]{Items: []Subscription{{ID: 10, UserID: 8, GroupID: 9, Status: "active", StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour)}}, Page: 1, PageSize: 1000, Total: 1, Pages: 1})
		case "/api/v1/admin/subscriptions/bulk-action":
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") == "" {
				t.Error("reset missing method or idempotency")
			}
			var payload struct {
				IDs                    []int64 `json:"subscription_ids"`
				Action                 string  `json:"action"`
				Daily, Weekly, Monthly bool
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if len(payload.IDs) != 1 || payload.IDs[0] != 10 || payload.Action != "reset_quota" || !payload.Weekly || payload.Daily || payload.Monthly {
				t.Error("unexpected reset payload", payload)
			}
			resets.Add(1)
			write(ResetResult{SuccessCount: 1, Results: []ResetItem{{SubscriptionID: 10, Success: true}}})
		default:
			forbidden.Add(1)
			t.Error("unexpected request", r.URL.Path)
			writeError(w, 404, "missing")
		}
	}))
	defer mainSite.Close()
	dir := t.TempDir()
	store, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.BaseURL = mainSite.URL
	cfg.AdminAPIKey = "integration-admin-key"
	cfg.AutoResetEnabled = true
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "fake", ChatID: "fake"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.Rules = []Rule{{ID: "rule", Name: "Rule", Enabled: true, AccountIDs: []int64{1}, SubscriptionIDs: []int64{10}, SubscriptionRefs: map[string]SubscriptionRef{"10": {UserID: 8, GroupID: 9}}, AutoReset: true, Weekly: true, NotifyTelegram: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store, nil)
	engine.wait = func(context.Context, time.Duration) error { return nil }
	engine.Notify = func(context.Context, Config, string, string) error { notices.Add(1); return nil }
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 0 {
		t.Fatal("baseline caused reset")
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 1 || notices.Load() != 1 {
		t.Fatal("confirmed transition did not execute once", resets.Load(), notices.Load())
	}
	state, err := store.Snapshot()
	if err != nil || len(state.Events) != 1 || len(state.Actions) != 1 || state.Actions[0].Status != "succeeded" {
		t.Fatal("persistent outcomes missing", state.Actions, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine = NewEngine(store, nil)
	engine.Notify = func(context.Context, Config, string, string) error { notices.Add(1); return nil }
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 1 || notices.Load() != 1 {
		t.Fatal("restart replayed reset or delivered notification")
	}
	if forbidden.Load() != 0 {
		t.Fatal("monitoring requested a forbidden upstream-triggering route")
	}
}
