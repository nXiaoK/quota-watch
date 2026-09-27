package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelAuthenticationCSRFAndSecretRedaction(t *testing.T) {
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	cfg.AdminAPIKey = "sensitive-admin"
	cfg.Telegram.BotToken = "sensitive-token"
	cfg.Email.Password = "sensitive-smtp"
	cfg.Telegram.ProxyURL = "http://user:sensitive-proxy@proxy.example"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(context.Background(), store, NewEngine(store, nil), nil, "admin", "panel-password")
	handler := server.Handler()
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/config", nil))
	if unauth.Code != 401 {
		t.Fatal("config exposed without auth")
	}
	_, cookie := loginForTest(t, handler, nil)
	request := httptest.NewRequest("GET", "/api/config", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, secret := range []string{cfg.AdminAPIKey, cfg.Telegram.BotToken, cfg.Email.Password, cfg.Telegram.ProxyURL} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("GET config leaked secret")
		}
	}
	for _, test := range []struct{ origin, header string }{{"", ""}, {"https://attacker.example", "1"}} {
		request := httptest.NewRequest("PUT", "http://localhost/api/rules", strings.NewReader(`{"rules":[]}`))
		request.AddCookie(cookie)
		request.Header.Set("Origin", test.origin)
		request.Header.Set("X-Quota-Watch", test.header)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatal("cross-site write accepted")
		}
	}
	request = httptest.NewRequest("PUT", "http://localhost/api/config", strings.NewReader(`{"base_url":"","poll_interval_seconds":60,"confirm_delay_seconds":5,"concurrency":3,"telegram":{},"email":{"tls_mode":"starttls"}}`))
	request.AddCookie(cookie)
	request.Header.Set("X-Quota-Watch", "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	loaded, _ := store.Config()
	if loaded.AdminAPIKey != cfg.AdminAPIKey {
		t.Fatal("redacted save deleted saved admin credential")
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest("GET", "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatal("healthcheck unavailable")
	}
	var body map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil || len(body) != 1 {
		t.Fatal("healthcheck exposed state")
	}
}
