package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVerboseLoggingSettingPersistsAndChangesExistingClient(t *testing.T) {
	var calls atomic.Int32
	mainSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/admin/accounts" {
			t.Error("logger changed requested API")
		}
		writeJSON(w, 200, map[string]any{"code": 0, "data": Page[Account]{Items: []Account{}, Page: 1, PageSize: 1}})
	}))
	defer mainSite.Close()
	dir := t.TempDir()
	store, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.BaseURL = mainSite.URL
	cfg.AdminAPIKey = "secret-admin-canary"
	if store.VerboseLoggingEnabled() || cfg.VerboseLogging {
		t.Fatal("logging should default off")
	}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	server := NewServer(context.Background(), store, NewEngine(store, logger), logger, "admin", "local-password")
	client, err := server.client()
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"page": {"1"}, "page_size": {"1"}}
	if _, err := client.ListAccounts(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatal("disabled setting emitted request logs")
	}
	cfg.VerboseLogging = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListAccounts(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "/api/v1/admin/accounts") || !strings.Contains(logs.String(), "http_status=200") || strings.Contains(logs.String(), cfg.AdminAPIKey) {
		t.Fatal("enabled diagnostics missing or leaked credential", logs.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.VerboseLoggingEnabled() {
		t.Fatal("logging preference did not survive restart")
	}
	cfg, err = store.Config()
	if err != nil || !cfg.VerboseLogging || publicConfig(cfg)["verbose_logging"] != true {
		t.Fatal("logging setting is missing from API", err)
	}
	server = NewServer(context.Background(), store, NewEngine(store, logger), logger, "admin", "local-password")
	client, err = server.client()
	if err != nil {
		t.Fatal(err)
	}
	cfg.VerboseLogging = false
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if _, err := client.ListAccounts(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 || calls.Load() != 3 {
		t.Fatal("toggle changed request count or failed to stop diagnostics", calls.Load(), logs.String())
	}
}
