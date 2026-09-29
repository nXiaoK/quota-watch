package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testAdminClient(t *testing.T, handler http.HandlerFunc) (*AdminClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewAdminClient(Config{BaseURL: server.URL, AdminAPIKey: "administrator-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func writeAdminJSON(t *testing.T, writer http.ResponseWriter, data any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": data}); err != nil {
		t.Error(err)
	}
}

func TestAdminClientBaseURLAndRedirectProtection(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	client, server := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/accounts" || r.Header.Get("x-api-key") != "administrator-secret" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		if r.URL.Query().Get("redirect") == "true" {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		writeAdminJSON(t, w, Page[Account]{Items: []Account{}, Page: 1, PageSize: 1})
	})
	for _, base := range []string{server.URL, server.URL + "/", server.URL + "/api/v1", server.URL + "/api/v1/"} {
		current, err := NewAdminClient(Config{BaseURL: base, AdminAPIKey: "administrator-secret"})
		if err != nil {
			t.Fatal(err)
		}
		info, err := current.Connection(context.Background())
		if err != nil || info.Version != "" || info.Message == "" {
			t.Fatalf("connection: %#v %v", info, err)
		}
	}
	var ignored map[string]any
	err := client.request(context.Background(), http.MethodGet, "/admin/accounts", url.Values{"redirect": {"true"}}, nil, "", &ignored, false)
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect was followed: error=%v visits=%d", err, redirected.Load())
	}
	if strings.Contains(err.Error(), "administrator-secret") || strings.Contains(err.Error(), target.URL) {
		t.Fatal("request error leaked sensitive transport information")
	}
	for _, base := range []string{"https://user:password@example.com", "https://example.com/?secret=x", "https://example.com/#fragment", "https://example.com/a/../", "https://example.com/a%2f.."} {
		if _, err := NewAdminClient(Config{BaseURL: base, AdminAPIKey: "secret"}); err == nil {
			t.Errorf("invalid base URL accepted: %s", base)
		}
	}
}

func TestAccountResponsesExcludeCredentialsAndUnknownExtra(t *testing.T) {
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("platform") != "openai" || r.URL.Query().Get("type") != "oauth" || r.URL.Query().Get("lite") != "true" || r.URL.Query().Get("include_scheduler_score") != "false" {
			t.Errorf("selector filters missing")
		}
		writeAdminJSON(t, w, map[string]any{"items": []any{map[string]any{
			"id": 1, "name": "account", "platform": "openai", "type": "oauth", "quota_dimension": "global",
			"credentials": map[string]string{"access_token": "upstream-private"},
			"extra":       map[string]any{"auto_reset_credit_enabled": true, "auto_reset_credit_7d_threshold": 95, "codex_token": "upstream-private", "unrecognized": map[string]string{"password": "upstream-private"}},
		}}, "page": 1, "page_size": 20, "pages": 1, "total": 1})
	})
	page, err := client.ListAccounts(context.Background(), nil)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list: %v %#v", err, page)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "upstream-private") || strings.Contains(string(encoded), "credentials") {
		t.Fatal("account response exposed credentials")
	}
	if len(page.Items[0].Extra) != 2 || page.Items[0].Extra["auto_reset_credit_enabled"] != true || page.Items[0].QuotaDimension != "global" {
		t.Fatalf("required metadata missing: %#v", page.Items[0])
	}
}

func storedAccountFixture(percent any) map[string]any {
	return map[string]any{"id": 9, "name": "Codex", "platform": "openai", "type": "oauth", "quota_dimension": "global",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": time.Now().UTC().Format(time.RFC3339),
		"credentials": map[string]any{"chatgpt_account_id": "upstream-account", "plan_type": "plus", "access_token": "do-not-expose"},
		"extra":       map[string]any{"codex_7d_used_percent": percent, "codex_usage_updated_at": "2026-09-20T10:00:00Z", "codex_7d_reset_at": "2026-09-27T10:00:00Z", "codex_7d_window_minutes": 10080}}
}

func TestStoredQuotaOnlyReadsAccountDetailsAndKeepsExplicitZero(t *testing.T) {
	var requests atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/admin/accounts/9" || r.URL.RawQuery != "" {
			t.Errorf("stored quota requested a forbidden endpoint: %s", r.URL.String())
			w.WriteHeader(http.StatusForbidden)
			return
		}
		percent := 24
		if requests.Add(1) > 1 {
			percent = 0
		}
		writeAdminJSON(t, w, storedAccountFixture(percent))
	})
	account, err := client.GetAccount(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.ReadStoredQuota(account)
	if err != nil || first.UsedPercent != 24 || first.Dimension != "global" || first.Source != "stored" {
		t.Fatalf("first quota: %#v %v", first, err)
	}
	account, err = client.GetAccount(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.ReadStoredQuota(account)
	if err != nil || second.UsedPercent != 0 || requests.Load() != 2 {
		t.Fatalf("second quota: %#v %v count=%d", second, err, requests.Load())
	}
	if second.Identity == "" || strings.Contains(second.Identity, "upstream-account") || second.Plan != "plus" || second.FetchedAt != 1789898400 {
		t.Fatal("quota identity or time missing")
	}
	if _, err = client.ReadStoredQuota(account); err != nil || requests.Load() != 2 {
		t.Fatal("stored quota parsing performed a network request")
	}
	encoded, _ := json.Marshal(account)
	if strings.Contains(string(encoded), "do-not-expose") || strings.Contains(string(encoded), "credentials") || strings.Contains(string(encoded), "codex_7d_used_percent") {
		t.Fatal("account public JSON included private snapshot or credentials")
	}
}

func TestStoredQuotaRejectsUnknownAndInvalidWindowsWithoutFallback(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing percentage", func(q map[string]any) {
			delete(q["extra"].(map[string]any), "codex_7d_used_percent")
		}},
		{"null percentage", func(q map[string]any) {
			q["extra"].(map[string]any)["codex_7d_used_percent"] = nil
		}},
		{"null extra", func(q map[string]any) { q["extra"] = nil }},
		{"wrong duration", func(q map[string]any) {
			q["extra"].(map[string]any)["codex_7d_window_minutes"] = 1440
		}},
		{"negative percentage", func(q map[string]any) {
			q["extra"].(map[string]any)["codex_7d_used_percent"] = -1
		}},
		{"missing identity", func(q map[string]any) { delete(q["credentials"].(map[string]any), "chatgpt_account_id") }},
		{"missing plan", func(q map[string]any) { delete(q["credentials"].(map[string]any), "plan_type") }},
		{"missing sampled time", func(q map[string]any) { delete(q["extra"].(map[string]any), "codex_usage_updated_at") }},
		{"missing reset", func(q map[string]any) {
			delete(q["extra"].(map[string]any), "codex_7d_reset_at")
		}},
		{"string percentage", func(q map[string]any) {
			q["extra"].(map[string]any)["codex_7d_used_percent"] = "0"
		}},
		{"bad sampled time", func(q map[string]any) { q["extra"].(map[string]any)["codex_usage_updated_at"] = "invalid" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/api/v1/admin/accounts/9" {
					t.Error("unknown snapshot caused forbidden upstream fallback")
				}
				q := storedAccountFixture(0)
				test.change(q)
				writeAdminJSON(t, w, q)
			})
			account, err := client.GetAccount(context.Background(), 9)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ReadStoredQuota(account)
			var unavailable *SnapshotUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatal("unknown quota was accepted as an observation")
			}
			if requests.Load() != 1 {
				t.Fatal("unknown snapshot performed extra network requests")
			}
		})
	}
}

func TestStoredQuotaSparkUsesShadowRowSnapshot(t *testing.T) {
	var requests atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/v1/admin/accounts/9" {
			t.Error("Spark read tried to refresh parent or upstream")
		}
		q := storedAccountFixture(12)
		q["parent_account_id"] = 5
		q["quota_dimension"] = "spark"
		q["credentials"] = map[string]any{}
		q["parent_chatgpt_account_id"] = "parent-account"
		q["parent_plan_type"] = "pro"
		writeAdminJSON(t, w, q)
	})
	account, err := client.GetAccount(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	q, err := client.ReadStoredQuota(account)
	if err != nil || q.Dimension != "spark" || q.UsedPercent != 12 || q.Plan != "pro" || requests.Load() != 1 {
		t.Fatalf("Spark quota was mixed with global: %#v %v", q, err)
	}
}

func TestStoredQuotaExpiredResetDoesNotPredictZero(t *testing.T) {
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := storedAccountFixture(51.5)
		q["extra"].(map[string]any)["codex_7d_reset_at"] = "2025-01-01T00:00:00Z"
		writeAdminJSON(t, w, q)
	})
	account, err := client.GetAccount(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	q, err := client.ReadStoredQuota(account)
	if err != nil || q.UsedPercent != 51.5 || q.ResetAt != 1735689600 {
		t.Fatalf("expired stored reset was altered: %#v %v", q, err)
	}
}

func TestAdminRequestAllowlistBlocksUpstreamAndOtherActions(t *testing.T) {
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) { visits.Add(1); w.WriteHeader(500) })
	for _, route := range []string{"/admin/openai/accounts/9/quota", "/admin/accounts/9/usage", "/admin/accounts/9/refresh", "/admin/accounts/9/test", "/admin/openai/accounts/9/reset-quota", "/admin/system/rollback-versions", "/admin/accounts/../settings"} {
		var result any
		if err := client.request(context.Background(), http.MethodGet, route, nil, nil, "", &result, false); err == nil {
			t.Errorf("forbidden route accepted: %s", route)
		}
	}
	var result any
	if err := client.request(context.Background(), http.MethodPost, "/admin/subscriptions/bulk-action", nil, subscriptionResetRequest{SubscriptionIDs: []int64{1}, Action: "revoke", Weekly: true}, "key", &result, true); err == nil {
		t.Fatal("unrelated mutation accepted")
	}
	if visits.Load() != 0 {
		t.Fatal("request allowlist performed a forbidden request")
	}
}

func TestGetSubscriptionsStopsAndUsesKnownScope(t *testing.T) {
	var calls atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/admin/subscriptions" || r.URL.Query().Get("page_size") != "1000" {
			t.Errorf("unexpected subscription request")
		}
		page := 1
		items := []Subscription{{ID: 1, UserID: 2, GroupID: 3}}
		pages := 10
		if r.URL.Query().Get("page") == "2" {
			page = 2
			items = []Subscription{{ID: 8, UserID: 42, GroupID: 3}, {ID: 9, UserID: 42, GroupID: 3}}
		}
		if calls.Load() > 2 {
			if r.URL.Query().Get("user_id") != "42" {
				t.Errorf("known target scope not used: %s", r.URL.RawQuery)
			}
			pages = 1
			items = []Subscription{{ID: 8, UserID: 42, GroupID: 3}, {ID: 9, UserID: 42, GroupID: 3}}
		}
		writeAdminJSON(t, w, Page[Subscription]{Items: items, Page: page, PageSize: 1000, Pages: pages, Total: 10000})
	})
	for run := 0; run < 2; run++ {
		found, err := client.GetSubscriptions(context.Background(), []int64{8, 9, 8})
		if err != nil || len(found) != 2 {
			t.Fatalf("get subscriptions: %#v %v", found, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("scanned beyond selected targets: %d calls", calls.Load())
	}
}

func TestPrimedSubscriptionTargetsUseRelatedGroupsOnly(t *testing.T) {
	var calls atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		group := r.URL.Query().Get("group_id")
		if group == "" && r.URL.Query().Get("user_id") == "" {
			t.Error("known subscription targets caused an unrestricted scan")
		}
		items := []Subscription{{ID: 8, UserID: 1, GroupID: 20}, {ID: 9, UserID: 2, GroupID: 20}}
		if r.URL.Query().Get("user_id") == "3" {
			items = []Subscription{{ID: 10, UserID: 3, GroupID: 30}}
		} else if group != "20" {
			t.Errorf("unexpected scope: %s", r.URL.RawQuery)
		}
		writeAdminJSON(t, w, Page[Subscription]{Items: items, Page: 1, PageSize: 1000, Pages: 15, Total: 15000})
	})
	client.PrimeSubscriptionHints(map[string]SubscriptionRef{
		"8": {UserID: 1, GroupID: 20}, "9": {UserID: 2, GroupID: 20}, "10": {UserID: 3, GroupID: 30},
	})
	found, err := client.GetSubscriptions(context.Background(), []int64{8, 9, 10})
	if err != nil || len(found) != 3 || calls.Load() != 2 {
		t.Fatalf("related scopes: %#v %v calls=%d", found, err, calls.Load())
	}
}

func TestResetSubscriptionsClassifiesUncertainOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		unknown bool
		retry   time.Duration
	}{
		{"upstream 500", 500, `{"message":"administrator-secret"}`, true, 0},
		{"rate limited", 429, `{"message":"administrator-secret"}`, false, 12 * time.Second},
		{"permission denied", 403, `{"message":"administrator-secret"}`, false, 0},
		{"request in progress", 409, `{"reason":"IDEMPOTENCY_IN_PROGRESS"}`, true, 0},
		{"invalid success envelope", 200, `{"data":{}}`, true, 0},
		{"invalid success JSON", 200, `not-json administrator-secret`, true, 0},
		{"missing result", 200, `{"code":0,"data":{"success_count":0,"failed_count":0,"results":[]}}`, true, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/subscriptions/bulk-action" || r.Header.Get("Idempotency-Key") != "event-key" {
					t.Errorf("unexpected reset request")
				}
				if test.retry > 0 {
					w.Header().Set("Retry-After", "12")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			})
			_, err := client.ResetSubscriptions(context.Background(), []int64{4}, ResetMask{Weekly: true}, "event-key")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Unknown != test.unknown || apiErr.RetryAfter != test.retry {
				t.Fatalf("outcome: %#v %v", apiErr, err)
			}
			if strings.Contains(err.Error(), "administrator-secret") {
				t.Fatal("API failure leaked credentials")
			}
		})
	}
	client, server := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {})
	server.Close()
	_, err := client.ResetSubscriptions(context.Background(), []int64{4}, ResetMask{Weekly: true}, "event-key")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Unknown {
		t.Fatalf("network outcome is not unknown: %v", err)
	}
}

func TestResetSubscriptionsPreservesPartialResultsWithoutRemoteErrorText(t *testing.T) {
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["action"] != "reset_quota" || body["weekly"] != true {
			t.Error("reset payload mismatch")
		}
		if _, exists := body["monthly"]; exists {
			t.Error("unselected quota window included")
		}
		writeAdminJSON(t, w, ResetResult{SuccessCount: 1, FailedCount: 1, Results: []ResetItem{{SubscriptionID: 4, Success: true}, {SubscriptionID: 5, Error: "administrator-secret remote-token"}}})
	})
	result, err := client.ResetSubscriptions(context.Background(), []int64{4, 5}, ResetMask{Weekly: true}, "event-key")
	if err != nil || result.SuccessCount != 1 || result.FailedCount != 1 || len(result.Results) != 2 {
		t.Fatalf("partial reset: %#v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "administrator-secret") || strings.Contains(string(encoded), "remote-token") {
		t.Fatal("reset failure exposed remote secrets")
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if result := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); result != 30*time.Second {
		t.Fatalf("retry date: %v", result)
	}
}
