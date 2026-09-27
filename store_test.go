package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreEncryptedConfigPersistenceAndBaselineTransaction(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.BaseURL = "https://sub2api.example"
	cfg.AdminAPIKey = "private-admin-key-for-test"
	cfg.Telegram.BotToken = "123:private-bot-token-for-test"
	cfg.Email.Password = "private-smtp-password-for-test"
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Rules = []Rule{{ID: "one", Enabled: true}}
		state.Observations["1"] = Observation{AccountID: 1, Last: &QuotaSnapshot{UsedPercent: 70}}
		state.Events = append(state.Events, Event{ID: "committed"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Events = append(state.Events, Event{ID: "must-not-commit"})
		return errors.New("abort")
	}); err == nil {
		t.Fatal("failed mutation committed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"quota-watch.db", "quota-watch.db-wal"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{cfg.AdminAPIKey, cfg.Telegram.BotToken, cfg.Email.Password} {
			if bytes.Contains(content, []byte(secret)) {
				t.Fatalf("plaintext secret in %s", name)
			}
		}
	}
	s, err = OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.Config()
	if err != nil || loaded.AdminAPIKey != cfg.AdminAPIKey || loaded.Email.Password != cfg.Email.Password {
		t.Fatal("encrypted config did not survive restart", err)
	}
	state, err := s.Snapshot()
	if err != nil || len(state.Events) != 1 || state.Observations["1"].Last.UsedPercent != 70 {
		t.Fatal("atomic state did not survive restart", err)
	}
	changed := cfg
	changed.BaseURL = "https://other.example"
	if err := s.SaveConfig(changed); err != nil {
		t.Fatal(err)
	}
	state, _ = s.Snapshot()
	if state.Rules[0].Enabled || len(state.Observations) != 0 {
		t.Fatal("installation change kept enabled rules/baselines")
	}
}

func TestStoreRejectsSecondInstanceAndWrongKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenStore(dir, ""); err == nil {
		_ = second.Close()
		t.Fatal("second instance accepted")
	}
	cfg := DefaultConfig()
	cfg.AdminAPIKey = "retained"
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if second, err := OpenStore(dir, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		_ = second.Close()
		t.Fatal("wrong decryption key accepted")
	}
	s, err = OpenStore(dir, "")
	if err != nil {
		t.Fatal("failed open retained lock", err)
	}
	defer s.Close()
	cfg, err = s.Config()
	if err != nil || cfg.AdminAPIKey != "retained" {
		t.Fatal("failed open changed existing data")
	}
}

func TestMergeConfigPreservesSecretsAndExplicitClear(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminAPIKey = "keep-admin"
	cfg.Telegram.BotToken = "keep-token"
	cfg.Telegram.ProxyURL = "http://user:password@proxy.example:8080"
	cfg.Email.Password = "keep-smtp"
	input := ConfigInput{Config: DefaultConfig()}
	merged, err := mergeConfig(input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if merged.AdminAPIKey != cfg.AdminAPIKey || merged.Telegram.BotToken != cfg.Telegram.BotToken || merged.Email.Password != cfg.Email.Password || merged.Telegram.ProxyURL != cfg.Telegram.ProxyURL {
		t.Fatal("blank inputs erased credentials")
	}
	input.ClearAdminAPIKey = true
	input.ClearTelegramToken = true
	input.ClearSMTPPassword = true
	input.ClearTelegramProxy = true
	merged, err = mergeConfig(input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if merged.AdminAPIKey != "" || merged.Telegram.BotToken != "" || merged.Email.Password != "" || merged.Telegram.ProxyURL != "" {
		t.Fatal("explicit clear ignored")
	}
}

func TestRuleSaveRejectsStaleInstallationAndRearmsBaseline(t *testing.T) {
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = "https://first.example"
	cfg.AdminAPIKey = "first-key"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: "one", Name: "Rule", Enabled: true, AccountIDs: []int64{1}}
	if err := store.SaveRulesForConfig(cfg, []Rule{rule}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.Observations["1"] = Observation{Last: &QuotaSnapshot{UsedPercent: 90}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rule.Enabled = false
	if err := store.SaveRulesForConfig(cfg, []Rule{rule}); err != nil {
		t.Fatal(err)
	}
	rule.Enabled = true
	if err := store.SaveRulesForConfig(cfg, []Rule{rule}); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if _, exists := state.Observations["1"]; exists {
		t.Fatal("reactivation retained pre-disable baseline")
	}
	changed := cfg
	changed.BaseURL = "https://second.example"
	if err := store.SaveConfig(changed); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRulesForConfig(cfg, []Rule{rule}); err == nil {
		t.Fatal("old installation wrote enabled rules to new installation")
	}
	if err := store.UpdateForConfig(cfg, func(state *State) error { state.Events = append(state.Events, Event{ID: "stale"}); return nil }); err == nil {
		t.Fatal("old installation sample was committed")
	}
	state, _ = store.Snapshot()
	if state.Rules[0].Enabled || len(state.Events) != 0 {
		t.Fatal("stale operation altered new installation")
	}
}
