package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "quota_watch_session"
	sessionLifetime   = 12 * time.Hour
)

type panelAuth struct {
	username string
	password string
	mu       sync.Mutex
	sessions map[[sha256.Size]byte]time.Time
	now      func() time.Time
}

type sessionInfo struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func newPanelAuth(username, password string) *panelAuth {
	return &panelAuth{username: username, password: password, sessions: make(map[[sha256.Size]byte]time.Time), now: time.Now}
}

func (a *panelAuth) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, 400, "登录信息格式无效")
		return
	}
	providedUser, expectedUser := sha256.Sum256([]byte(input.Username)), sha256.Sum256([]byte(a.username))
	providedPassword, expectedPassword := sha256.Sum256([]byte(input.Password)), sha256.Sum256([]byte(a.password))
	validUser := subtle.ConstantTimeCompare(providedUser[:], expectedUser[:])
	validPassword := subtle.ConstantTimeCompare(providedPassword[:], expectedPassword[:])
	if validUser&validPassword != 1 || a.password == "" {
		writeError(w, 401, "用户名或密码错误")
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		writeError(w, 500, "登录暂时不可用，请稍后重试")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	now := a.now().UTC()
	expires := now.Add(sessionLifetime)
	a.mu.Lock()
	for key, expiry := range a.sessions {
		if !expiry.After(now) {
			delete(a.sessions, key)
		}
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
	}
	a.sessions[sha256.Sum256([]byte(token))] = expires
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime / time.Second), Expires: expires})
	writeJSON(w, 200, sessionInfo{Authenticated: true, Username: a.username, ExpiresAt: expires})
}

func (a *panelAuth) session(r *http.Request) (sessionInfo, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || len(cookie.Value) != 43 {
		return sessionInfo{}, false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[key]
	if !ok {
		return sessionInfo{}, false
	}
	if !expires.After(a.now()) {
		delete(a.sessions, key)
		return sessionInfo{}, false
	}
	return sessionInfo{Authenticated: true, Username: a.username, ExpiresAt: expires}, true
}

func (a *panelAuth) invalidate(r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
		a.mu.Unlock()
	}
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0).UTC()})
}

func sameHostOrigin(r *http.Request) (*url.URL, bool) {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || !strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return nil, false
	}
	return origin, true
}

func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if origin, ok := sameHostOrigin(r); ok && origin.Scheme == "https" {
		return true
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(address); ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-Proto") == "https" {
			return true
		}
	}
	return false
}
