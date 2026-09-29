package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSystemClientReadsUseExactRoutesAndDecodeResponses(t *testing.T) {
	usageAt := time.Date(2026, 9, 29, 1, 50, 0, 0, time.FixedZone("CST", 8*3600))
	var calls atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("x-api-key") != "administrator-secret" {
			t.Errorf("unexpected method or authentication: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v1/admin/usage":
			if r.URL.RawQuery != "page=1&page_size=1&sort_by=created_at&sort_order=desc" {
				t.Errorf("unexpected usage query: %q", r.URL.RawQuery)
			}
			writeAdminJSON(t, w, map[string]any{"items": []any{map[string]any{"created_at": usageAt}}, "total": 1, "page": 1, "page_size": 1, "pages": 1})
		case "/api/v1/admin/system/check-updates":
			if r.URL.RawQuery != "force=true" {
				t.Errorf("unexpected update query: %q", r.URL.RawQuery)
			}
			writeAdminJSON(t, w, map[string]any{"current_version": "1.2.3", "latest_version": "1.2.4", "has_update": true, "build_type": "release", "warning": "GitHub unavailable; cached data"})
		case "/api/v1/admin/system/version":
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected version query: %q", r.URL.RawQuery)
			}
			writeAdminJSON(t, w, map[string]any{"version": "1.2.3"})
		default:
			t.Errorf("unexpected read route: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	gotUsage, exists, err := client.LatestUsage(context.Background())
	if err != nil || !exists || !gotUsage.Equal(usageAt) {
		t.Fatalf("latest usage: %v %t %v", gotUsage, exists, err)
	}
	info, err := client.CheckUpdates(context.Background())
	if err != nil || info.CurrentVersion != "1.2.3" || info.LatestVersion != "1.2.4" || !info.HasUpdate || info.BuildType != "release" || info.Warning == "" {
		t.Fatalf("check updates: %#v %v", info, err)
	}
	version, err := client.RunningVersion(context.Background())
	if err != nil || version != "1.2.3" || calls.Load() != 3 {
		t.Fatalf("running version: %q %v calls=%d", version, err, calls.Load())
	}
}

func TestLatestUsageDistinguishesEmptyFromInvalidData(t *testing.T) {
	tests := []struct {
		name, body string
		invalid    bool
	}{
		{"no logs", `{"code":0,"data":{"items":[],"total":0,"page":1,"page_size":1,"pages":1}}`, false},
		{"empty despite positive total", `{"code":0,"data":{"items":[],"total":1,"page":1,"page_size":1,"pages":1}}`, true},
		{"missing timestamp", `{"code":0,"data":{"items":[{}],"total":1,"page":1,"page_size":1,"pages":1}}`, true},
		{"wrong timestamp", `{"code":0,"data":{"items":[{"created_at":"invalid"}],"total":1,"page":1,"page_size":1,"pages":1}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := testAdminClient(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tt.body) })
			_, exists, err := client.LatestUsage(context.Background())
			if tt.invalid {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Unknown || exists {
					t.Fatalf("invalid log was treated as idle: exists=%t error=%v", exists, err)
				}
			} else if err != nil || exists {
				t.Fatalf("empty log response: exists=%t error=%v", exists, err)
			}
		})
	}
}

func TestSystemClientMutationsUseExactRoutesAndIdempotencyKeys(t *testing.T) {
	var calls atomic.Int64
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.ContentLength != 0 || r.Header.Get("x-api-key") != "administrator-secret" {
			t.Errorf("unexpected system mutation: %s %s content-length=%d", r.Method, r.URL, r.ContentLength)
		}
		switch r.URL.Path {
		case "/api/v1/admin/system/update":
			if r.Header.Get("Idempotency-Key") != "update-20260929" {
				t.Error("update idempotency key missing")
			}
			writeAdminJSON(t, w, map[string]any{"message": "Update completed. Please restart the service.", "need_restart": true, "operation_id": "op-update"})
		case "/api/v1/admin/system/restart":
			if r.Header.Get("Idempotency-Key") != "restart-20260929" {
				t.Error("restart idempotency key missing")
			}
			writeAdminJSON(t, w, map[string]any{"message": "Service restart initiated", "operation_id": "op-restart"})
		default:
			t.Errorf("unexpected system mutation route: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	update, err := client.PerformSystemUpdate(context.Background(), "update-20260929")
	if err != nil || !update.NeedRestart || update.AlreadyUpToDate || update.OperationID != "op-update" {
		t.Fatalf("update response: %#v %v", update, err)
	}
	if err := client.RestartSystem(context.Background(), "restart-20260929"); err != nil || calls.Load() != 2 {
		t.Fatalf("restart response: %v calls=%d", err, calls.Load())
	}
}

func TestSystemClientUpdateNoopAndUnknownOutcomes(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		unknown    bool
		noop       bool
	}{
		{"already current", `{"code":0,"data":{"message":"Already up to date","already_up_to_date":true,"current_version":"1.2.4","latest_version":"1.2.4"}}`, 200, false, true},
		{"server failure", `{"message":"secret"}`, 500, true, false},
		{"rate limited", `{"message":"secret"}`, 429, false, false},
		{"idempotency in progress", `{"reason":"IDEMPOTENCY_IN_PROGRESS"}`, 409, true, false},
		{"operation lock busy", `{"reason":"SYSTEM_OPERATION_BUSY"}`, 409, false, false},
		{"invalid success", `{"code":0,"data":{}}`, 200, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := testAdminClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			})
			result, err := client.PerformSystemUpdate(context.Background(), "update-key")
			if tt.noop {
				if err != nil || !result.AlreadyUpToDate || result.NeedRestart {
					t.Fatalf("no-op update: %#v %v", result, err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Unknown != tt.unknown || apiErr.Status != tt.status {
				t.Fatalf("update classification: %#v %v", apiErr, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("remote response leaked into error")
			}
		})
	}
}

func TestSystemClientUpdateTimeoutAndCancellationAreUnknown(t *testing.T) {
	client, _ := testAdminClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := client.PerformSystemUpdate(ctx, "timeout-key")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Unknown {
		t.Fatalf("timed out update should have unknown outcome: %v", err)
	}

	originalTransport := client.http.Transport
	var remaining time.Duration
	client.http.Transport = systemDeadlineProbe{base: originalTransport, observe: func(r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("update request has no deadline")
			return
		}
		remaining = time.Until(deadline)
	}}
	client.http.Transport = systemStaticResponse{probe: client.http.Transport}
	_, err = client.PerformSystemUpdate(context.Background(), "deadline-key")
	if err != nil || remaining < 14*time.Minute || remaining > adminUpdateTimeout {
		t.Fatalf("update deadline: remaining=%v error=%v", remaining, err)
	}
}

type systemDeadlineProbe struct {
	base    http.RoundTripper
	observe func(*http.Request)
}

func (p systemDeadlineProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	p.observe(r)
	return p.base.RoundTrip(r)
}

type systemStaticResponse struct{ probe http.RoundTripper }

func (s systemStaticResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	if probe, ok := s.probe.(systemDeadlineProbe); ok {
		probe.observe(r)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"message":"Already up to date","already_up_to_date":true}}`))}, nil
}

func TestSystemClientAllowlistRejectsOtherRoutesAndInputs(t *testing.T) {
	var visits atomic.Int64
	client, _ := testAdminClient(t, func(http.ResponseWriter, *http.Request) { visits.Add(1) })
	blocked := []struct {
		method, route string
		query         url.Values
		body          any
		key           string
		mutation      bool
	}{
		{http.MethodGet, "/admin/system/check-updates", url.Values{"force": {"false"}}, nil, "", false},
		{http.MethodGet, "/admin/system/check-updates", url.Values{"force": {"true"}, "timezone": {"Asia/Shanghai"}}, nil, "", false},
		{http.MethodGet, "/admin/usage", nil, nil, "", false},
		{http.MethodPost, "/admin/system/rollback", nil, nil, "key", true},
		{http.MethodPost, "/admin/system/update", nil, map[string]any{"force": true}, "key", true},
		{http.MethodPost, "/admin/system/restart", url.Values{"force": {"true"}}, nil, "key", true},
		{http.MethodPost, "/admin/system/update", nil, nil, "", true},
	}
	for _, tt := range blocked {
		var result any
		if err := client.request(context.Background(), tt.method, tt.route, tt.query, tt.body, tt.key, &result, tt.mutation); err == nil {
			t.Errorf("disallowed request escaped allowlist: %s %s", tt.method, tt.route)
		}
	}
	if visits.Load() != 0 {
		t.Fatalf("disallowed request reached server %d times", visits.Load())
	}
	for _, key := range []string{"", "a b", "a\nInjected: secret", strings.Repeat("x", 129)} {
		if _, err := client.PerformSystemUpdate(context.Background(), key); err == nil {
			t.Errorf("invalid update key accepted: %q", key)
		}
		if err := client.RestartSystem(context.Background(), key); err == nil {
			t.Errorf("invalid restart key accepted: %q", key)
		}
	}
}
