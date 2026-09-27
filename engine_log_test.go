package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func engineDiagnosticLogger(engine *Engine, logs *bytes.Buffer) {
	engine.logger = slog.New(slog.NewJSONHandler(logs, nil))
}

func readEngineDiagnostics(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestEngineDiagnosticsExposeSnapshotStatesWithoutSecrets(t *testing.T) {
	for _, test := range []struct {
		status   string
		percent  float64
		unknown  bool
		apiError bool
	}{
		{status: "baseline", percent: 47},
		{status: "watching", percent: 51},
		{status: "waiting", percent: 42},
		{status: "unknown", unknown: true},
		{status: "error", apiError: true},
		{status: "reset"},
	} {
		t.Run(test.status, func(t *testing.T) {
			engine, store, api, _ := engineFixture(t)
			store.cfg.VerboseLogging = true
			store.cfg.AdminAPIKey = "CANARY-admin-key"
			store.cfg.Telegram.BotToken, store.cfg.Telegram.ChatID = "CANARY-bot-token", "919876543210"
			store.cfg.Email.Password, store.cfg.Email.Username = "CANARY-smtp-password", "CANARY-email@example.test"
			store.state.Rules[0].Name = "CANARY-rule-name"
			observation := store.state.Observations["1"]
			observation.AccountName, observation.Last.AccountName, observation.Last.Identity = "CANARY-account-name", "CANARY-account-name", "CANARY-identity"
			if test.status == "baseline" {
				observation.Last = nil
			}
			store.state.Observations["1"] = observation
			answer := engineQuotaAnswer{percent: test.percent, identity: "CANARY-identity"}
			if test.status == "waiting" {
				answer.fetchedAt = observation.Last.FetchedAt
			}
			if test.unknown {
				answer.err = &SnapshotUnavailableError{Message: "CANARY-response-body"}
			}
			if test.apiError {
				answer.err = &APIError{Status: 503, Message: "CANARY-response-body"}
			}
			api.answers[1] = []engineQuotaAnswer{answer}
			if test.status == "reset" {
				api.answers[1] = append(api.answers[1], answer)
			}
			var logs bytes.Buffer
			engineDiagnosticLogger(engine, &logs)
			if err := engine.CheckNow(context.Background()); (err != nil) != test.apiError {
				t.Fatalf("unexpected detection result: %v", err)
			}
			if strings.Contains(logs.String(), "CANARY") || strings.Contains(logs.String(), store.cfg.Telegram.ChatID) {
				t.Fatal("diagnostic logs exposed private state, configuration, or an untrusted error")
			}
			var snapshot map[string]any
			var confirmed, eventSaved, actionSucceeded bool
			for _, entry := range readEngineDiagnostics(t, &logs) {
				if entry["msg"] == "配额快照" {
					snapshot = entry
				}
				confirmed = confirmed || entry["msg"] == "归零快照复核" && entry["status"] == "confirmed"
				eventSaved = eventSaved || entry["msg"] == "归零事件已保存"
				actionSucceeded = actionSucceeded || entry["msg"] == "订阅重置动作" && entry["status"] == "succeeded" && entry["success_count"] == float64(2)
			}
			if snapshot == nil || snapshot["status"] != test.status || snapshot["account_id"] != float64(1) {
				t.Fatalf("missing snapshot state %q", test.status)
			}
			if test.unknown || test.apiError {
				if _, ok := snapshot["current_percent"]; ok {
					t.Fatal("an unavailable snapshot was logged as a current quota value")
				}
				if _, ok := snapshot["snapshot_sampled_at"]; ok {
					t.Fatal("an unavailable snapshot was logged with a current sampling time")
				}
			} else if snapshot["current_percent"] != test.percent || snapshot["snapshot_sampled_at"] == nil {
				t.Fatal("valid quota value or sampling time is missing")
			}
			if test.status == "baseline" {
				if _, ok := snapshot["previous_percent"]; ok {
					t.Fatal("a missing previous baseline was logged as zero")
				}
			} else if snapshot["previous_percent"] != float64(42) {
				t.Fatal("last valid quota value is missing")
			}
			if test.status == "reset" && (!confirmed || !eventSaved || !actionSucceeded || api.accountCalls[1] != 2 || len(api.resetCalls) != 1) {
				t.Fatal("confirmed reset diagnostics or existing request counts changed")
			}
		})
	}
}

func TestEngineDiagnosticsExplainInactiveCycles(t *testing.T) {
	for _, test := range []struct {
		status string
		mode   string
	}{
		{status: "no_enabled_rules", mode: "automatic"},
		{status: "no_accounts", mode: "manual"},
		{status: "config_invalid", mode: "manual"},
		{status: "backoff", mode: "automatic"},
	} {
		t.Run(test.status, func(t *testing.T) {
			engine, store, _, clock := engineFixture(t)
			store.cfg.VerboseLogging = true
			factoryCalls := 0
			factory := engine.NewAdmin
			engine.NewAdmin = func(cfg Config) (AdminAPI, error) {
				factoryCalls++
				if test.status == "config_invalid" {
					return nil, errors.New("CANARY-invalid-configuration")
				}
				return factory(cfg)
			}
			switch test.status {
			case "no_enabled_rules":
				store.state.Rules[0].Enabled = false
			case "no_accounts":
				store.state.Rules[0].AccountIDs = nil
			case "backoff":
				observation := store.state.Observations["1"]
				observation.NextCheckAt = clock.Now().Add(10 * time.Minute)
				store.state.Observations["1"] = observation
			}
			var logs bytes.Buffer
			engineDiagnosticLogger(engine, &logs)
			err := engine.checkNow(context.Background(), test.mode)
			if (err != nil) != (test.status == "config_invalid") {
				t.Fatal("unexpected detection result")
			}
			if strings.Contains(logs.String(), "CANARY") {
				t.Fatal("invalid configuration leaked through diagnostics")
			}
			var start, end bool
			for _, entry := range readEngineDiagnostics(t, &logs) {
				if entry["msg"] != "检测周期" || entry["mode"] != test.mode {
					continue
				}
				start = start || entry["phase"] == "start"
				if entry["phase"] == "end" && entry["status"] == test.status && entry["checked_accounts"] == float64(0) {
					end = true
					if test.status == "backoff" && entry["skipped_backoff"] != float64(1) {
						t.Fatal("backoff did not report the skipped account")
					}
				}
			}
			if !start || !end {
				t.Fatal("inactive cycle did not explain its state")
			}
			if (test.status == "no_enabled_rules" || test.status == "no_accounts") && factoryCalls != 0 {
				t.Fatal("diagnostics created a client without a monitored account")
			}
		})
	}
}

type diagnosticEngineStore struct {
	*memoryEngineStore
	enabled     atomic.Bool
	configReads atomic.Int64
}

func (s *diagnosticEngineStore) VerboseLoggingEnabled() bool { return s.enabled.Load() }

func (s *diagnosticEngineStore) Config() (Config, error) {
	s.configReads.Add(1)
	return s.memoryEngineStore.Config()
}

func TestEngineDiagnosticsRespectLiveSwitchAndBusyWithoutQueries(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.cfg.VerboseLogging = true
	live := &diagnosticEngineStore{memoryEngineStore: store}
	live.enabled.Store(true)
	engine.store = live
	var logs bytes.Buffer
	engineDiagnosticLogger(engine, &logs)
	engine.busy.Store(true)
	if err := engine.CheckNow(context.Background()); !errors.Is(err, errEngineBusy) || live.configReads.Load() != 0 {
		t.Fatal("busy diagnostics changed serialization or queried configuration")
	}
	if entries := readEngineDiagnostics(t, &logs); len(entries) != 1 || entries[0]["status"] != "busy" {
		t.Fatal("busy cycle was not explained")
	}
	engine.busy.Store(false)
	logs.Reset()
	api.answers[1] = []engineQuotaAnswer{{percent: 0}, {percent: 0}}
	engine.wait = func(_ context.Context, delay time.Duration) error {
		live.enabled.Store(false)
		clock.Add(delay)
		return nil
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, entry := range readEngineDiagnostics(t, &logs) {
		if entry["phase"] == "end" || entry["msg"] == "归零事件已保存" || entry["msg"] == "订阅重置动作" || entry["msg"] == "配额快照" {
			t.Fatal("an in-flight diagnostic was printed after disabling logs")
		}
	}
	if len(mustEngineState(t, store).Events) != 1 || len(api.resetCalls) != 1 {
		t.Fatal("disabling diagnostics changed reset execution")
	}
}

func TestEngineDiagnosticsDisabledPreserveStateAndRequests(t *testing.T) {
	var reference State
	var referenceConfigReads int64
	for _, enabled := range []bool{false, true} {
		engine, store, api, _ := engineFixture(t)
		store.cfg.VerboseLogging = enabled
		live := &diagnosticEngineStore{memoryEngineStore: store}
		live.enabled.Store(enabled)
		engine.store = live
		api.answers[1] = []engineQuotaAnswer{{percent: 0}, {percent: 0}}
		var logs bytes.Buffer
		engineDiagnosticLogger(engine, &logs)
		if err := engine.CheckNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		if api.accountCalls[1] != 2 || api.quotaCalls[1] != 2 || len(api.resetCalls) != 1 {
			t.Fatal("logging changed sampling or reset request counts")
		}
		state := mustEngineState(t, store)
		if !enabled {
			if logs.Len() != 0 {
				t.Fatal("disabled diagnostics printed output")
			}
			reference, referenceConfigReads = state, live.configReads.Load()
		} else if !reflect.DeepEqual(state, reference) || referenceConfigReads != live.configReads.Load() {
			t.Fatal("diagnostics changed durable state or configuration query counts")
		}
	}
}

func TestEngineDiagnosticsManualConfirmationPending(t *testing.T) {
	engine, store, api, _ := manualFixture(t)
	store.cfg.VerboseLogging = true
	api.answers[1] = []engineQuotaAnswer{{percent: 0}, {percent: 0}}
	var logs bytes.Buffer
	engineDiagnosticLogger(engine, &logs)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var pending bool
	for _, entry := range readEngineDiagnostics(t, &logs) {
		pending = pending || entry["msg"] == "手动重置确认待处理" && entry["status"] == "pending" && entry["subscriptions"] == float64(2)
	}
	if !pending || len(api.resetCalls) != 0 || len(mustEngineState(t, store).ManualRequests) != 1 {
		t.Fatal("manual confirmation diagnostics changed approval or reset behavior")
	}
}
