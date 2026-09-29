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
	"time"
)

func TestUpdaterAdminRequestsRespectDetailedLogToggle(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			mainSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("x-api-key") != "administrator-secret" {
					t.Error("administrator credential was not sent")
				}
				w.Header().Set("Set-Cookie", "session=upstream-private")
				switch r.URL.Path {
				case "/api/v1/admin/usage":
					writeAdminJSON(t, w, map[string]any{"items": []any{}, "total": 0, "page": 1, "page_size": 1, "pages": 1})
				case "/api/v1/admin/system/check-updates":
					if r.URL.RawQuery != "force=true" {
						t.Errorf("unexpected version check query: %q", r.URL.RawQuery)
					}
					writeAdminJSON(t, w, map[string]any{"current_version": "1.2.3", "latest_version": "1.2.4", "has_update": true, "build_type": "release"})
				case "/api/v1/admin/system/update":
					if r.Header.Get("Idempotency-Key") != "idempotency-private" {
						t.Error("update idempotency key was not sent")
					}
					writeAdminJSON(t, w, map[string]any{"message": "updated", "need_restart": true})
				case "/api/v1/admin/system/restart":
					if r.Header.Get("Idempotency-Key") != "idempotency-private" {
						t.Error("restart idempotency key was not sent")
					}
					writeAdminJSON(t, w, map[string]any{"message": "restarting"})
				case "/api/v1/admin/system/version":
					writeAdminJSON(t, w, map[string]any{"version": "1.2.4"})
				default:
					t.Errorf("unexpected administrator route: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
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
			cfg.AdminAPIKey = "administrator-secret"
			cfg.VerboseLogging = enabled
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var buffer bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buffer, nil))
			updater := NewUpdater(store, NewEngine(store, logger), logger)
			api, err := updater.newClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			client, ok := api.(*AdminClient)
			if !ok {
				t.Fatalf("updater created %T instead of an administrator client", api)
			}
			if _, _, err := client.LatestUsage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := client.CheckUpdates(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := client.PerformSystemUpdate(context.Background(), "idempotency-private"); err != nil {
				t.Fatal(err)
			}
			if err := client.RestartSystem(context.Background(), "idempotency-private"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.RunningVersion(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 5 {
				t.Fatalf("expected five administrator calls, got %d", calls.Load())
			}
			entries := adminLogEntries(t, &buffer)
			if !enabled {
				if len(entries) != 0 {
					t.Fatalf("disabled detailed logging emitted request entries: %#v", entries)
				}
				return
			}
			if len(entries) != 10 {
				t.Fatalf("five administrator calls need start/end entries, got %#v", entries)
			}
			want := []struct{ method, path string }{
				{http.MethodGet, "/api/v1/admin/usage"},
				{http.MethodGet, "/api/v1/admin/system/check-updates"},
				{http.MethodPost, "/api/v1/admin/system/update"},
				{http.MethodPost, "/api/v1/admin/system/restart"},
				{http.MethodGet, "/api/v1/admin/system/version"},
			}
			for i, expected := range want {
				start, end := entries[i*2], entries[i*2+1]
				for _, entry := range []map[string]any{start, end} {
					if entry["source"] != "auto_update" || entry["method"] != expected.method || entry["path"] != expected.path {
						t.Errorf("missing automatic update request details: %#v", entry)
					}
				}
				if start["phase"] != "start" || end["phase"] != "end" || start["request_id"] == nil || start["request_id"] != end["request_id"] || end["outcome"] != "success" || end["http_status"] != float64(http.StatusOK) {
					t.Errorf("uncorrelated or unsuccessful request entries: %#v %#v", start, end)
				}
				if i == 1 {
					for _, entry := range []map[string]any{start, end} {
						query, err := url.ParseQuery(entry["query"].(string))
						if err != nil || query.Get("force") != "true" {
							t.Errorf("forced update check was not safely logged: %#v, %v", entry, err)
						}
					}
				}
			}
			assertNoRequestSecrets(t, &buffer)
			if strings.Contains(buffer.String(), "idempotency-private") {
				t.Fatal("automatic update request log exposed its idempotency key")
			}
		})
	}
}

func TestUpdaterPhaseLogsRespectDetailedLogToggle(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeUpdateAPI{
				info: testUpdateInfo(), usageAt: now.Add(-11 * time.Minute), usageExists: true,
				updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
			}
			updater, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
			cfg, err := store.Config()
			if err != nil {
				t.Fatal(err)
			}
			cfg.VerboseLogging = enabled
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var buffer bytes.Buffer
			updater.logger = slog.New(slog.NewJSONHandler(&buffer, nil))
			if err := updater.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			var phases = make(map[string]bool)
			for _, entry := range adminLogEntries(t, &buffer) {
				if entry["msg"] != "Sub2API 自动更新" {
					continue
				}
				if !enabled {
					t.Fatalf("disabled detailed logging emitted update phase: %#v", entry)
				}
				phase, ok := entry["phase"].(string)
				if !ok || entry["status"] == nil {
					t.Errorf("update phase lacks phase or status: %#v", entry)
				}
				phases[phase] = true
			}
			if enabled {
				for _, phase := range []string{"version_check", "idle_check", "update", "restart", "version_verify"} {
					if !phases[phase] {
						t.Errorf("missing automatic update phase %q: %v", phase, phases)
					}
				}
			}
		})
	}
}

func TestDetailedRequestLogForceQueryOnlyExposesExpectedValue(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		want   string
	}{
		{"forced check", []string{"true"}, "true"},
		{"unexpected value", []string{"false"}, "[redacted]"},
		{"multiple values", []string{"true", "administrator-secret"}, "[redacted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logged := safeRequestLogQuery(url.Values{"force": test.values})
			query, err := url.ParseQuery(logged)
			if err != nil || query.Get("force") != test.want || strings.Contains(logged, "administrator-secret") {
				t.Fatalf("unsafe forced check query: %q, error=%v", logged, err)
			}
		})
	}
}
