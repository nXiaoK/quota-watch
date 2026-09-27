package main

import (
	"reflect"
	"testing"
)

func TestManualDecisionStoreRejectsChangedTelegramAuthority(t *testing.T) {
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = "https://main.example"
	cfg.AdminAPIKey = "admin-key"
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "42"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.ManualRequests = []ManualRequest{{ID: "test", Status: "pending"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.Telegram.ChatID = "43"
	if err := store.SaveConfig(changed); err != nil {
		t.Fatal(err)
	}
	called := false
	err = store.UpdateForManualConfig(cfg, func(state *State) error { called = true; state.ManualRequests[0].Status = "processing"; return nil })
	if err == nil || called {
		t.Fatal("stale chat authority approved a persisted manual request")
	}
	state, _ := store.Snapshot()
	if state.ManualRequests[0].Status != "pending" {
		t.Fatal("rejected authority changed manual request")
	}
	if err := store.UpdateForManualConfig(changed, func(state *State) error { state.ManualRequests[0].Status = "ignored"; return nil }); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Snapshot()
	if state.ManualRequests[0].Status != "ignored" {
		t.Fatal("valid private decision did not persist")
	}
}

func TestTelegramUserPermissionConfigMerge(t *testing.T) {
	previous := DefaultConfig()
	previous.Telegram.AllowedUserIDs = []int64{9, 5}
	input := ConfigInput{Config: DefaultConfig()}
	merged, err := mergeConfig(input, previous)
	if err != nil || !reflect.DeepEqual(merged.Telegram.AllowedUserIDs, []int64{5, 9}) {
		t.Fatal("omitted field did not preserve permissions", err)
	}
	input.Telegram.AllowedUserIDs = []int64{9, 5, 9}
	merged, err = mergeConfig(input, previous)
	if err != nil || !reflect.DeepEqual(merged.Telegram.AllowedUserIDs, []int64{5, 9}) {
		t.Fatal("permission list did not normalize", err)
	}
	input.Telegram.AllowedUserIDs = []int64{}
	merged, err = mergeConfig(input, previous)
	if err != nil || len(merged.Telegram.AllowedUserIDs) != 0 {
		t.Fatal("explicit clear did not remove permissions", err)
	}
	input.Telegram.AllowedUserIDs = []int64{-1}
	if _, err = mergeConfig(input, previous); err == nil {
		t.Fatal("invalid Telegram user ID accepted")
	}
}
