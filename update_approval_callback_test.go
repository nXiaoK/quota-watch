package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	updateCallbackID    = "0123456789abcdef0123456789abcdef"
	newUpdateCallbackID = "fedcba9876543210fedcba9876543210"
)

func updateCallbackFixture(t *testing.T, chatID string, allowed []int64) (*Engine, *memoryEngineStore, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.BaseURL, cfg.AdminAPIKey = "https://sub2api.example", "test-admin-key"
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-bot", ChatID: chatID, AllowedUserIDs: allowed}
	cfg.Update.NotifyAvailableTelegramEnabled = true
	store := &memoryEngineStore{cfg: cfg, state: State{Update: UpdateState{
		CurrentVersion: "v0.2.9", LatestVersion: "v0.2.10", HasUpdate: true, Status: "awaiting_approval",
	}}}
	store.state.UpdateApproval = testUpdateApproval(cfg, updateCallbackID, "v0.2.10", now)
	engine := newEngine(store, nil)
	engine.Now = func() time.Time { return now }
	return engine, store, now
}

func testUpdateApproval(cfg Config, id, version string, now time.Time) UpdateApproval {
	return UpdateApproval{
		ID: id, Version: version, Status: "pending", ChatID: cfg.Telegram.ChatID,
		ConnectionFingerprint: manualConnectionFingerprint(cfg),
		TelegramFingerprint:   manualTelegramFingerprint(cfg.Telegram),
		CreatedAt:             now, ExpiresAt: now.Add(time.Hour),
	}
}

func updateVersionCallback(id, choice string, chatID, fromID int64, chatType, username string) TelegramCallback {
	return TelegramCallback{
		QueryID: "callback", ChatID: chatID, ChatUsername: username, ChatType: chatType,
		FromID: fromID, MessageID: 1, Data: "qw:u" + choice + ":" + id,
	}
}

func TestUpdateVersionPrivateChoiceIsAuthorizedAndIdempotent(t *testing.T) {
	for _, choice := range []string{"a", "r"} {
		t.Run(choice, func(t *testing.T) {
			engine, store, _ := updateCallbackFixture(t, "42", []int64{99})
			callback := updateVersionCallback(updateCallbackID, choice, 42, 99, "private", "")
			decision, err := engine.DecideUpdateVersion(context.Background(), callback)
			if err != nil || decision.RequestID != "" || store.state.UpdateApproval.Status != "pending" {
				t.Fatalf("an allowed outsider controlled a private-chat decision: %+v, %v", decision, err)
			}
			callback.FromID = 42
			callback.ChatID = 99
			decision, err = engine.DecideUpdateVersion(context.Background(), callback)
			if err != nil || decision.RequestID != "" || store.state.UpdateApproval.Status != "pending" {
				t.Fatalf("a callback from another chat controlled the decision: %+v, %v", decision, err)
			}
			callback.ChatID = 42
			decision, err = engine.DecideUpdateVersion(context.Background(), callback)
			if err != nil || decision.RequestID != updateCallbackID || store.state.UpdateApproval.DecisionAt.IsZero() {
				t.Fatalf("private recipient's choice was not saved: %+v, %v", decision, err)
			}
			wantedStatus, wantedUpdateStatus, wantedActor := "approved", "queued", int64(42)
			if choice == "r" {
				wantedStatus, wantedUpdateStatus, wantedActor = "declined", "declined", 0
			}
			if store.state.UpdateApproval.Status != wantedStatus || store.state.Update.Status != wantedUpdateStatus || store.state.UpdateApproval.ApprovedBy != wantedActor {
				t.Fatalf("choice was saved with wrong state: %+v, %+v", store.state.UpdateApproval, store.state.Update)
			}
			firstDecisionAt := store.state.UpdateApproval.DecisionAt
			otherChoice := "r"
			if choice == "r" {
				otherChoice = "a"
			}
			repeated := updateVersionCallback(updateCallbackID, otherChoice, 42, 42, "private", "")
			decision, err = engine.DecideUpdateVersion(context.Background(), repeated)
			if err != nil || decision.RequestID != updateCallbackID || store.state.UpdateApproval.Status != wantedStatus ||
				store.state.Update.Status != wantedUpdateStatus || store.state.UpdateApproval.ApprovedBy != wantedActor ||
				!store.state.UpdateApproval.DecisionAt.Equal(firstDecisionAt) {
				t.Fatalf("repeated or opposite click changed the durable choice: %+v, %v", decision, err)
			}
		})
	}
}

func TestUpdateVersionGroupChoiceRequiresAllowedActorAndChat(t *testing.T) {
	for _, tc := range []struct {
		name, configuredChat, username, wrongUsername string
		chatID                                        int64
	}{
		{"numeric group", "-100123", "", "", -100123},
		{"named group", "@QuotaGroup", "quotagroup", "anothergroup", -100123},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, _ := updateCallbackFixture(t, tc.configuredChat, []int64{7})
			callback := updateVersionCallback(updateCallbackID, "a", tc.chatID, 8, "supergroup", tc.username)
			unauthorizedCallbacks := []TelegramCallback{
				callback,
				updateVersionCallback(updateCallbackID, "a", tc.chatID, 7, "private", tc.username),
			}
			if tc.wrongUsername == "" {
				unauthorizedCallbacks = append(unauthorizedCallbacks, updateVersionCallback(updateCallbackID, "a", -100999, 7, "supergroup", tc.username))
			}
			for _, unauthorized := range unauthorizedCallbacks {
				decision, err := engine.DecideUpdateVersion(context.Background(), unauthorized)
				if err != nil || decision.RequestID != "" || store.state.UpdateApproval.Status != "pending" {
					t.Fatalf("unauthorized group callback changed decision: %+v, %v", decision, err)
				}
			}
			if tc.wrongUsername != "" {
				wrongName := updateVersionCallback(updateCallbackID, "a", tc.chatID, 7, "supergroup", tc.wrongUsername)
				decision, err := engine.DecideUpdateVersion(context.Background(), wrongName)
				if err != nil || decision.RequestID != "" || store.state.UpdateApproval.Status != "pending" {
					t.Fatalf("another group username changed decision: %+v, %v", decision, err)
				}
			}
			allowed := updateVersionCallback(updateCallbackID, "a", tc.chatID, 7, "supergroup", tc.username)
			decision, err := engine.DecideUpdateVersion(context.Background(), allowed)
			if err != nil || decision.RequestID != updateCallbackID || store.state.UpdateApproval.Status != "approved" || store.state.UpdateApproval.ApprovedBy != 7 {
				t.Fatalf("allowed group member could not approve: %+v, %v", decision, err)
			}
		})
	}
}

func TestUpdateVersionOldCallbacksCannotChangeCurrentRelease(t *testing.T) {
	engine, store, now := updateCallbackFixture(t, "42", nil)
	store.state.Update.LatestVersion = "v0.2.11"
	store.state.UpdateApproval = testUpdateApproval(store.cfg, newUpdateCallbackID, "v0.2.11", now)
	old := updateVersionCallback(updateCallbackID, "a", 42, 42, "private", "")
	decision, err := engine.DecideUpdateVersion(context.Background(), old)
	if err != nil || decision.RequestID != "" || store.state.UpdateApproval.Status != "pending" || store.state.UpdateApproval.ID != newUpdateCallbackID {
		t.Fatalf("old button changed the new release choice: %+v, %v", decision, err)
	}
	current := updateVersionCallback(newUpdateCallbackID, "a", 42, 42, "private", "")
	store.state.Update.LatestVersion = "v0.2.12"
	decision, err = engine.DecideUpdateVersion(context.Background(), current)
	if err != nil || decision.RequestID != newUpdateCallbackID || store.state.UpdateApproval.Status != "invalid" || store.state.Update.Status == "queued" {
		t.Fatalf("approval for a superseded version was accepted: %+v, %v", decision, err)
	}
}

func TestUpdateVersionConfigChangesInvalidateApproval(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"site":            func(cfg *Config) { cfg.BaseURL = "https://other.example" },
		"admin key":       func(cfg *Config) { cfg.AdminAPIKey = "other-admin-key" },
		"bot":             func(cfg *Config) { cfg.Telegram.BotToken = "456:other-bot" },
		"allowed users":   func(cfg *Config) { cfg.Telegram.AllowedUserIDs = []int64{7} },
		"notice disabled": func(cfg *Config) { cfg.Update.NotifyAvailableTelegramEnabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			engine, store, _ := updateCallbackFixture(t, "42", nil)
			change(&store.cfg)
			callback := updateVersionCallback(updateCallbackID, "a", 42, 42, "private", "")
			decision, err := engine.DecideUpdateVersion(context.Background(), callback)
			if err != nil || decision.RequestID != updateCallbackID || store.state.UpdateApproval.Status != "invalid" ||
				store.state.Update.Status == "queued" || strings.Contains(decision.Text, "已加入") {
				t.Fatalf("changed configuration accepted an old approval: %+v, %v", decision, err)
			}
		})
	}
}
