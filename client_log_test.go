package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func captureAdminRequestLogs(client *AdminClient) *bytes.Buffer {
	buffer := &bytes.Buffer{}
	client.logger = slog.New(slog.NewJSONHandler(buffer, nil))
	return buffer
}

func adminLogEntries(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid request log: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertNoRequestSecrets(t *testing.T, buffer *bytes.Buffer) {
	t.Helper()
	for _, secret := range []string{"administrator-secret", "search-private", "query-private", "upstream-private", "transport-private", "idempotency-private", "Cookie", "x-api-key", "Idempotency-Key", "subscription_ids"} {
		if strings.Contains(buffer.String(), secret) {
			t.Errorf("request log exposed %q", secret)
		}
	}
}

func TestAdminRequestLogsDefaultOff(t *testing.T) {
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		visits.Add(1)
		writeAdminJSON(t, w, Page[Account]{Items: []Account{}, Page: 1, PageSize: 1})
	})
	if client.logger != slog.Default() {
		t.Fatal("administrator client did not inherit the process logger")
	}
	buffer := captureAdminRequestLogs(client)
	if _, err := client.Connection(context.Background()); err != nil {
		t.Fatal(err)
	}
	var result any
	if err := client.request(context.Background(), http.MethodGet, "/admin/accounts/9/refresh", nil, nil, "", &result, false); err == nil {
		t.Fatal("forbidden request was allowed while logging was off")
	}
	if buffer.Len() != 0 || visits.Load() != 1 {
		t.Fatalf("disabled request logging changed behavior: logs=%s visits=%d", buffer.String(), visits.Load())
	}
}

func TestAdminRequestLogsGetPathQueryAndRedaction(t *testing.T) {
	var visits atomic.Int64
	var buffer *bytes.Buffer
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		visits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/api/v1/admin/accounts" || r.Header.Get("x-api-key") != "administrator-secret" {
			t.Error("request logging changed the administrator request")
		}
		if r.URL.Query().Get("search") != "search-private administrator-secret" || r.URL.Query().Get("unknown-secret") != "query-private" {
			t.Error("request logging altered query values")
		}
		if !strings.Contains(buffer.String(), `"phase":"start"`) {
			t.Error("request start was not logged before the service was contacted")
		}
		w.Header().Set("Set-Cookie", "session=upstream-private")
		writeAdminJSON(t, w, map[string]any{"items": []any{}, "page": 1, "page_size": 20, "credentials": map[string]string{"access_token": "upstream-private"}})
	})
	client.baseURL.Path = "/proxy/api/v1"
	client.logEnabled = func() bool { return true }
	buffer = captureAdminRequestLogs(client)
	query := url.Values{"page": {"1"}, "page_size": {"20"}, "group": {"7"}, "status": {"active"}, "sort_by": {"name"}, "sort_order": {"desc"}, "lite": {"true"}, "search": {"search-private administrator-secret"}, "unknown-secret": {"query-private"}}
	var result Page[Account]
	if err := client.request(context.Background(), http.MethodGet, "/admin/accounts", query, nil, "", &result, false); err != nil {
		t.Fatal(err)
	}
	entries := adminLogEntries(t, buffer)
	if len(entries) != 2 || visits.Load() != 1 {
		t.Fatalf("unexpected logging or request count: entries=%d visits=%d", len(entries), visits.Load())
	}
	start, end := entries[0], entries[1]
	if start["phase"] != "start" || end["phase"] != "end" || start["request_id"] == "" || start["request_id"] != end["request_id"] {
		t.Fatalf("request entries cannot be correlated: %#v", entries)
	}
	for _, entry := range entries {
		if entry["level"] != "INFO" || entry["method"] != http.MethodGet || entry["path"] != "/proxy/api/v1/admin/accounts" || entry["host"] != client.baseURL.Host {
			t.Fatalf("missing request details: %#v", entry)
		}
		loggedQuery, err := url.ParseQuery(entry["query"].(string))
		if err != nil || loggedQuery.Get("page") != "1" || loggedQuery.Get("page_size") != "20" || loggedQuery.Get("group") != "7" || loggedQuery.Get("platform") != "openai" || loggedQuery.Get("type") != "oauth" || loggedQuery.Get("include_scheduler_score") != "false" || loggedQuery.Get("search") != "[redacted]" || loggedQuery.Has("unknown-secret") {
			t.Fatalf("unsafe or incomplete logged query: %#v error=%v", loggedQuery, err)
		}
	}
	if end["http_status"] != float64(http.StatusOK) || end["outcome"] != "success" || end["duration_ms"] == nil || end["error"] != nil {
		t.Fatalf("incorrect successful result: %#v", end)
	}
	assertNoRequestSecrets(t, buffer)
}

func TestAdminRequestLogsPostExcludeResetBodyAndIdempotencyKey(t *testing.T) {
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		visits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/subscriptions/bulk-action" || r.Header.Get("Idempotency-Key") != "idempotency-private" {
			t.Error("request logging changed the reset request")
		}
		writeAdminJSON(t, w, ResetResult{SuccessCount: 1, Results: []ResetItem{{SubscriptionID: 4, Success: true, Error: "upstream-private"}}})
	})
	client.logEnabled = func() bool { return true }
	buffer := captureAdminRequestLogs(client)
	result, err := client.ResetSubscriptions(context.Background(), []int64{4}, ResetMask{Weekly: true}, "idempotency-private")
	if err != nil || result.SuccessCount != 1 || visits.Load() != 1 {
		t.Fatalf("reset behavior changed: %#v error=%v visits=%d", result, err, visits.Load())
	}
	entries := adminLogEntries(t, buffer)
	if len(entries) != 2 || entries[1]["method"] != http.MethodPost || entries[1]["path"] != "/api/v1/admin/subscriptions/bulk-action" || entries[1]["outcome"] != "success" {
		t.Fatalf("reset request details missing: %#v", entries)
	}
	assertNoRequestSecrets(t, buffer)
}

func TestAdminRequestLogsHTTPAndProtocolFailures(t *testing.T) {
	tests := []struct {
		name, content, outcome, class string
		status                        int
	}{
		{"HTTP rejection", `{"message":"upstream-private administrator-secret"}`, "http", "http_status", http.StatusForbidden},
		{"malformed JSON", `upstream-private administrator-secret`, "protocol", "response_envelope", http.StatusOK},
		{"unsuccessful envelope", `{"code":1,"data":{"token":"upstream-private"}}`, "protocol", "response_envelope", http.StatusOK},
		{"invalid payload", `{"code":0,"data":"upstream-private"}`, "protocol", "response_payload", http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var visits atomic.Int64
			client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
				visits.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.content)
			})
			client.logEnabled = func() bool { return true }
			buffer := captureAdminRequestLogs(client)
			if _, err := client.ListAccounts(context.Background(), nil); err == nil {
				t.Fatal("invalid administrator response was accepted")
			}
			entries := adminLogEntries(t, buffer)
			if len(entries) != 2 || visits.Load() != 1 || entries[1]["outcome"] != test.outcome || entries[1]["error_class"] != test.class || entries[1]["http_status"] != float64(test.status) || entries[1]["error"] == "" {
				t.Fatalf("incorrect failure details: %#v visits=%d", entries, visits.Load())
			}
			assertNoRequestSecrets(t, buffer)
		})
	}
}

func TestAdminRequestLogsTransportErrorsAreRedacted(t *testing.T) {
	tests := []struct {
		name, outcome, class string
		context              func() (context.Context, context.CancelFunc)
	}{
		{"transport error", "transport", "connection", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"canceled request", "canceled", "context_canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{"expired request", "canceled", "deadline_exceeded", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testAdminClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected real service request") })
			client.logEnabled = func() bool { return true }
			buffer := captureAdminRequestLogs(client)
			var attempts atomic.Int64
			client.http.Transport = notificationRoundTripper(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				return nil, errors.New("transport-private " + request.URL.String() + " administrator-secret")
			})
			ctx, cancel := test.context()
			defer cancel()
			if _, err := client.ListAccounts(ctx, url.Values{"search": {"search-private"}}); err == nil {
				t.Fatal("failed administrator request was accepted")
			}
			entries := adminLogEntries(t, buffer)
			if len(entries) != 2 || attempts.Load() != 1 || entries[1]["outcome"] != test.outcome || entries[1]["error_class"] != test.class || entries[1]["http_status"] != float64(0) {
				t.Fatalf("incorrect transport details: %#v attempts=%d", entries, attempts.Load())
			}
			assertNoRequestSecrets(t, buffer)
		})
	}
}

func TestAdminRequestLogsRejectedRoutesNeverExposeInputOrSendRequests(t *testing.T) {
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(http.ResponseWriter, *http.Request) { visits.Add(1) })
	client.logEnabled = func() bool { return true }
	buffer := captureAdminRequestLogs(client)
	var result any
	for _, request := range []struct{ method, route string }{
		{http.MethodGet, "/admin/accounts/9/refresh"},
		{http.MethodGet, "/admin/openai/accounts/9/quota"},
		{http.MethodGet, "/admin/accounts/administrator-secret"},
		{"transport-private", "/admin/subscriptions"},
	} {
		if err := client.request(context.Background(), request.method, request.route, url.Values{"search": {"search-private"}}, nil, "", &result, false); err == nil {
			t.Fatal("forbidden administrator request was accepted")
		}
	}
	entries := adminLogEntries(t, buffer)
	if len(entries) != 8 || visits.Load() != 0 {
		t.Fatalf("logging bypassed route protection: entries=%d visits=%d", len(entries), visits.Load())
	}
	for _, entry := range entries {
		if entry["path"] != "[blocked]" || entry["query"] != nil || (entry["phase"] == "end" && entry["outcome"] != "rejected") {
			t.Fatalf("rejected request exposed untrusted details: %#v", entry)
		}
	}
	if entries[6]["method"] != "[blocked]" || entries[7]["method"] != "[blocked]" {
		t.Fatal("invalid HTTP method was logged without redaction")
	}
	assertNoRequestSecrets(t, buffer)
}

func TestAdminRequestLogsUseCurrentToggleAndStopInFlightEnd(t *testing.T) {
	var enabled atomic.Bool
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		if visits.Add(1) == 2 {
			enabled.Store(false)
		}
		writeAdminJSON(t, w, Page[Account]{Items: []Account{}, Page: 1, PageSize: 1})
	})
	client.logEnabled = enabled.Load
	buffer := captureAdminRequestLogs(client)
	if _, err := client.Connection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() != 0 {
		t.Fatal("disabled current toggle was ignored")
	}
	enabled.Store(true)
	if _, err := client.Connection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries := adminLogEntries(t, buffer); len(entries) != 1 || entries[0]["phase"] != "start" {
		t.Fatalf("request end was logged after disabling: %#v", entries)
	}
	enabled.Store(true)
	if _, err := client.Connection(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries := adminLogEntries(t, buffer)
	if len(entries) != 3 || entries[1]["phase"] != "start" || entries[2]["phase"] != "end" || entries[0]["request_id"] == entries[1]["request_id"] || visits.Load() != 3 {
		t.Fatalf("toggle did not apply to the next request: %#v visits=%d", entries, visits.Load())
	}
}

func TestSafeRequestLogQueryOnlyIncludesValidatedValues(t *testing.T) {
	logged := safeRequestLogQuery(url.Values{
		"page": {"query-private"}, "page_size": {"1", "query-private"}, "user_id": {"42"}, "group_id": {"7"}, "group": {"ungrouped"},
		"status": {"query-private"}, "sort_by": {"query-private"}, "sort_order": {"query-private"}, "platform": {"query-private"}, "type": {"query-private"},
		"lite": {"query-private"}, "include_scheduler_score": {"query-private"}, "search": {"query-private"}, "query-private": {"query-private"},
	})
	values, err := url.ParseQuery(logged)
	if err != nil || values.Get("user_id") != "42" || values.Get("group_id") != "7" || values.Get("group") != "ungrouped" || values.Has("query-private") || strings.Contains(logged, "query-private") {
		t.Fatalf("unsafe query values were logged: %s error=%v", logged, err)
	}
	for _, name := range []string{"page", "page_size", "status", "sort_by", "sort_order", "platform", "type", "lite", "include_scheduler_score", "search"} {
		if values.Get(name) != "[redacted]" {
			t.Errorf("unvalidated %s was logged: %s", name, logged)
		}
	}
}
