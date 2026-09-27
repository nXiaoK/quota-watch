package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func loginForTest(t *testing.T, handler http.Handler, existing *http.Cookie) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/auth/login", strings.NewReader(`{"username":"admin","password":"panel-password"}`))
	request.Header.Set("X-Quota-Watch", "1")
	request.Header.Set("Origin", "http://localhost")
	if existing != nil {
		request.AddCookie(existing)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value == "" {
		t.Fatal("login did not issue the session cookie")
	}
	return response, cookies[0]
}

func TestSessionAuthenticationLifecycle(t *testing.T) {
	server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	server.auth.now = func() time.Time { return now }
	handler := server.Handler()
	session := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://localhost/api/auth/session", nil)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	unauthenticated := session(nil)
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("unauthenticated API must return 401 without a browser authentication challenge")
	}
	if unauthenticated.Header().Get("Set-Cookie") != "" {
		t.Fatal("a delayed unauthorized response must not clear a newly issued session cookie")
	}
	response, cookie := loginForTest(t, handler, nil)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || cookie.Secure || cookie.MaxAge != int(sessionLifetime/time.Second) {
		t.Fatalf("invalid session cookie flags: %+v", cookie)
	}
	bytes, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(bytes) != 32 {
		t.Fatal("session must contain 32 random bytes")
	}
	var info sessionInfo
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil || !info.Authenticated || info.Username != "admin" || !info.ExpiresAt.Equal(now.Add(sessionLifetime)) || !cookie.Expires.Equal(info.ExpiresAt) {
		t.Fatal("invalid login response", response.Body.String())
	}
	if _, ok := server.auth.sessions[sha256.Sum256([]byte(cookie.Value))]; !ok {
		t.Fatal("session must be stored under its token hash")
	}
	if session(cookie).Code != http.StatusOK {
		t.Fatal("session cookie failed to authenticate")
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost/api/auth/session", nil)
	request.SetBasicAuth("admin", "panel-password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("Basic authentication must not bypass the session or open a browser prompt")
	}
	_, replacement := loginForTest(t, handler, cookie)
	if replacement.Value == cookie.Value || session(cookie).Code != http.StatusUnauthorized || session(replacement).Code != http.StatusOK {
		t.Fatal("successful login must rotate and invalidate the previous session")
	}
	logout := httptest.NewRequest(http.MethodPost, "http://localhost/api/auth/logout", nil)
	logout.AddCookie(replacement)
	logout.Header.Set("X-Quota-Watch", "1")
	logout.Header.Set("Origin", "http://localhost")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, logout)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"authenticated":false}` {
		t.Fatal("logout failed", response.Code, response.Body.String())
	}
	deleted := response.Result().Cookies()
	if len(deleted) != 1 || deleted[0].Name != sessionCookieName || deleted[0].Value != "" || deleted[0].MaxAge != -1 || !deleted[0].HttpOnly || deleted[0].SameSite != http.SameSiteStrictMode || deleted[0].Path != "/" {
		t.Fatal("logout did not delete the protected session cookie")
	}
	if session(replacement).Code != http.StatusUnauthorized || len(server.auth.sessions) != 0 {
		t.Fatal("logout left the session valid on the server")
	}
	_, expiring := loginForTest(t, handler, nil)
	now = now.Add(sessionLifetime - time.Second)
	response = session(expiring)
	if response.Code != http.StatusOK {
		t.Fatal("session expired before its absolute deadline")
	}
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil || !info.ExpiresAt.Equal(expiring.Expires) {
		t.Fatal("session access changed the absolute expiry")
	}
	now = now.Add(time.Second)
	if session(expiring).Code != http.StatusUnauthorized || len(server.auth.sessions) != 0 {
		t.Fatal("session must expire at its deadline and be removed")
	}
	_, fresh := loginForTest(t, handler, nil)
	restarted := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	request = httptest.NewRequest(http.MethodGet, "http://localhost/api/auth/session", nil)
	request.AddCookie(fresh)
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("sessions must be invalid after restart")
	}
}

func TestLoginRejectsInvalidCredentialsAndBodies(t *testing.T) {
	server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	handler := server.Handler()
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"wrong username", `{"username":"unknown","password":"panel-password"}`, http.StatusUnauthorized},
		{"wrong password", `{"username":"admin","password":"wrong"}`, http.StatusUnauthorized},
		{"missing credentials", `{}`, http.StatusUnauthorized},
		{"invalid JSON", `{`, http.StatusBadRequest},
		{"multiple values", `{"username":"admin","password":"panel-password"}{}`, http.StatusBadRequest},
		{"oversized body", `{"username":"admin","password":"` + strings.Repeat("a", 1024*1024) + `"}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://localhost/api/auth/login", strings.NewReader(test.body))
			request.Header.Set("X-Quota-Watch", "1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get("WWW-Authenticate") != "" || len(response.Result().Cookies()) != 0 || len(server.auth.sessions) != 0 {
				t.Fatal("invalid login accepted or issued an authentication challenge", response.Code, response.Body.String())
			}
			if test.status == http.StatusUnauthorized && strings.TrimSpace(response.Body.String()) != `{"error":"用户名或密码错误"}` {
				t.Fatal("credential errors must not reveal which credential failed")
			}
			if strings.Contains(response.Body.String(), "panel-password") {
				t.Fatal("response leaked a credential")
			}
		})
	}
}

func TestAuthenticationCSRFGuards(t *testing.T) {
	server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	handler := server.Handler()
	_, cookie := loginForTest(t, handler, nil)
	for _, path := range []string{"/api/auth/login", "/api/auth/logout", "/api/rules"} {
		for _, test := range []struct{ name, origin, header string }{
			{"missing validation header", "http://localhost", ""},
			{"foreign origin", "https://attacker.example", "1"},
			{"opaque origin", "null", "1"},
			{"invalid origin scheme", "file://localhost", "1"},
			{"origin with user info", "http://admin@localhost", "1"},
			{"origin with path", "http://localhost/path", "1"},
		} {
			t.Run(path+" "+test.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(`{"username":"admin","password":"panel-password"}`))
				request.AddCookie(cookie)
				request.Header.Set("Origin", test.origin)
				request.Header.Set("X-Quota-Watch", test.header)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatal("cross-site authentication or write accepted", response.Code)
				}
			})
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost/api/auth/session", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal("rejected logout invalidated the session")
	}
}

func TestSessionCookieSecureForHTTPSAndTrustedProxy(t *testing.T) {
	for _, test := range []struct {
		name, target, origin, remoteAddress, forwardedProto string
		secure                                              bool
	}{
		{"direct HTTP", "http://localhost/api/auth/login", "http://localhost", "192.0.2.1:1000", "", false},
		{"direct TLS", "https://localhost/api/auth/login", "", "192.0.2.1:1000", "http", true},
		{"local HTTPS proxy", "http://localhost/api/auth/login", "", "127.0.0.1:1000", "https", true},
		{"local IPv6 HTTPS proxy", "http://localhost/api/auth/login", "", "[::1]:1000", "https", true},
		{"untrusted forwarded header", "http://localhost/api/auth/login", "", "192.0.2.1:1000", "https", false},
		{"Docker HTTPS proxy", "http://localhost/api/auth/login", "https://localhost", "172.18.0.1:1000", "https", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(`{"username":"admin","password":"panel-password"}`))
			request.RemoteAddr = test.remoteAddress
			request.Header.Set("X-Quota-Watch", "1")
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-Forwarded-Proto", test.forwardedProto)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			cookies := response.Result().Cookies()
			if response.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Secure != test.secure {
				t.Fatal("incorrect secure cookie handling", response.Code, response.Header().Get("Set-Cookie"))
			}
		})
	}
}

func TestOnlyStaticPagesAndHealthArePublic(t *testing.T) {
	server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	handler := server.Handler()
	for _, path := range []string{"/", "/login", "/healthz"} {
		request := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if (response.Code != http.StatusOK && !(path != "/healthz" && response.Code == http.StatusServiceUnavailable)) || response.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("public page requires authentication", path, response.Code)
		}
	}
	for _, path := range []string{"/api", "/api/config", "/api/state", "/api/accounts", "/api/subscriptions", "/api/auth/session", "/api/unknown"} {
		request := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("API endpoint is exposed or requests Basic authentication", path, response.Code)
		}
	}
}

func TestConcurrentSessionLookupAndInvalidation(t *testing.T) {
	server := NewServer(context.Background(), nil, nil, nil, "admin", "panel-password")
	_, cookie := loginForTest(t, server.Handler(), nil)
	request := httptest.NewRequest(http.MethodGet, "http://localhost/api/auth/session", nil)
	request.AddCookie(cookie)
	var jobs sync.WaitGroup
	for range 20 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			for range 20 {
				server.auth.session(request)
			}
		}()
	}
	server.auth.invalidate(request)
	jobs.Wait()
	if _, ok := server.auth.session(request); ok {
		t.Fatal("concurrent session access restored an invalidated session")
	}
}
