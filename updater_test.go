package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeUpdateAPI struct {
	info         SystemUpdateInfo
	usageAt      time.Time
	usageExists  bool
	usageErr     error
	updateResult SystemUpdateResult
	updateErr    error
	restartErr   error
	version      string
	afterUsage   func(int)
	checks       int
	usageReads   int
	updates      int
	restarts     int
}

func (f *fakeUpdateAPI) LatestUsage(context.Context) (time.Time, bool, error) {
	f.usageReads++
	if f.afterUsage != nil {
		f.afterUsage(f.usageReads)
	}
	return f.usageAt, f.usageExists, f.usageErr
}
func (f *fakeUpdateAPI) CheckUpdates(context.Context) (SystemUpdateInfo, error) {
	f.checks++
	return f.info, nil
}
func (f *fakeUpdateAPI) PerformSystemUpdate(_ context.Context, _ string) (SystemUpdateResult, error) {
	f.updates++
	return f.updateResult, f.updateErr
}
func (f *fakeUpdateAPI) RestartSystem(_ context.Context, _ string) error {
	f.restarts++
	if f.restartErr == nil {
		f.version = f.info.LatestVersion
	}
	return f.restartErr
}
func (f *fakeUpdateAPI) RunningVersion(context.Context) (string, error) { return f.version, nil }

func newTestUpdater(t *testing.T, settings UpdateConfig, now time.Time, fake *fakeUpdateAPI) (*Updater, *Store) {
	t.Helper()
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := DefaultConfig()
	cfg.BaseURL = "https://sub2api.example"
	cfg.AdminAPIKey = "administrator-secret"
	cfg.Update = settings
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	u := NewUpdater(store, NewEngine(store, nil), nil)
	u.now = func() time.Time { return now }
	u.newClient = func(Config) (updateAPI, error) { return fake, nil }
	return u, store
}

func testUpdateInfo() SystemUpdateInfo {
	return SystemUpdateInfo{CurrentVersion: "1.2.3", LatestVersion: "1.2.4", HasUpdate: true, BuildType: "release"}
}

func TestUpdaterIdleUpdateAndRestartOnlyOnce(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{
		info: testUpdateInfo(), usageAt: now.Add(-11 * time.Minute), usageExists: true,
		updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
	}
	u, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.updates != 1 || fake.restarts != 1 || fake.usageReads != 2 || state.Update.Status != "success" || state.Update.CurrentVersion != "1.2.4" {
		t.Fatalf("unexpected update result: calls=%d/%d/%d state=%+v", fake.updates, fake.restarts, fake.usageReads, state.Update)
	}
	if err := u.Tick(context.Background()); err != nil || fake.updates != 1 {
		t.Fatalf("successful update repeated: %v calls=%d", err, fake.updates)
	}
}

func TestUpdaterScheduledWaitsForIdleAndSkipsAfterWindow(t *testing.T) {
	now := time.Date(2026, 9, 28, 18, 5, 0, 0, time.UTC) // 02:05 in Shanghai
	fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now.Add(-5 * time.Minute), usageExists: true}
	u, store := newTestUpdater(t, UpdateConfig{ScheduledEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if state.Update.Status != "waiting_idle" || fake.updates != 0 {
		t.Fatalf("scheduled update ignored activity: %+v calls=%d", state.Update, fake.updates)
	}
	u.now = func() time.Time { return now.Add(time.Hour) } // 03:05 in Shanghai
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.updates != 0 {
		t.Fatal("scheduled update executed after its window")
	}
}

func TestUpdaterScheduledWindowEndsBeforeUpdateRequest(t *testing.T) {
	inside := time.Date(2026, 9, 28, 18, 59, 59, 0, time.UTC) // 02:59:59 in Shanghai
	outside := inside.Add(time.Second)
	fake := &fakeUpdateAPI{
		info: testUpdateInfo(), usageAt: inside.Add(-time.Hour), usageExists: true,
		updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
	}
	u, store := newTestUpdater(t, UpdateConfig{ScheduledEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, inside, fake)
	clockReads := 0
	u.now = func() time.Time {
		clockReads++
		if clockReads >= 6 {
			return outside
		}
		return inside
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.updates != 0 || fake.restarts != 0 || state.Update.Status != "available" || !state.Update.LastAttemptAt.IsZero() {
		t.Fatalf("update started after the scheduled window ended: calls=%d/%d state=%+v", fake.updates, fake.restarts, state.Update)
	}
}

func TestUpdaterDoesNotStartAfterAutomaticUpdateIsDisabled(t *testing.T) {
	now := time.Date(2026, 9, 28, 18, 30, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true, version: "1.2.3"}
	u, store := newTestUpdater(t, UpdateConfig{ScheduledEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	fake.afterUsage = func(read int) {
		if read != 2 {
			return
		}
		cfg, err := store.Config()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Update.ScheduledEnabled = false
		if err := store.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.updates != 0 || fake.restarts != 0 || state.Update.Status == "updating" || !state.Update.LastAttemptAt.IsZero() {
		t.Fatalf("update started after the setting was disabled: calls=%d/%d state=%+v", fake.updates, fake.restarts, state.Update)
	}
}

func TestUpdaterEmptyUsageNeedsTenMinutesOfObservation(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{info: testUpdateInfo(), updateResult: SystemUpdateResult{AlreadyUpToDate: true}, version: "1.2.3"}
	u, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if fake.updates != 0 || !state.Update.EmptySince.Equal(now) || state.Update.Status != "waiting_idle" {
		t.Fatalf("empty usage was treated as immediately idle: %+v calls=%d", state.Update, fake.updates)
	}
	u.now = func() time.Time { return now.Add(9 * time.Minute) }
	if err := u.Tick(context.Background()); err != nil || fake.updates != 0 {
		t.Fatalf("updated before ten minutes: %v calls=%d", err, fake.updates)
	}
	u.now = func() time.Time { return now.Add(10 * time.Minute) }
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.updates != 1 || fake.restarts != 0 {
		t.Fatalf("empty usage did not become idle after ten minutes: updates=%d restarts=%d", fake.updates, fake.restarts)
	}
}

func TestUpdaterNewVersionDoesNotReuseOldEmptyObservation(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{info: testUpdateInfo(), updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3"}
	u, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := store.Update(func(state *State) error {
		state.Update.EmptySince = now.Add(-time.Hour)
		state.Update.Status = "up_to_date"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.updates != 0 || state.Update.Status != "waiting_idle" || !state.Update.EmptySince.Equal(now) {
		t.Fatalf("new release reused stale empty observation: calls=%d state=%+v", fake.updates, state.Update)
	}
}

func TestUpdaterWarningFailsClosedAndClearsStaleCandidate(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true}
	fake.info.Warning = "GitHub unavailable; cached data"
	u, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := store.Update(func(s *State) error {
		s.Update.HasUpdate = true
		s.Update.LatestVersion = "1.2.4"
		s.Update.LastCheckAt = now.Add(-7 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(context.Background()); err == nil {
		t.Fatal("warning was accepted as an update check")
	}
	state, _ := store.Snapshot()
	if state.Update.HasUpdate || state.Update.Status != "check_failed" || fake.updates != 0 || fake.usageReads != 0 {
		t.Fatalf("stale candidate used after warning: %+v calls=%d", state.Update, fake.updates)
	}
}

func TestUpdaterUnknownUpdateNeverRestartsOrReplays(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{
		info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true,
		updateErr: &APIError{Message: "administrator API request was canceled or timed out", Unknown: true}, version: "1.2.3",
	}
	u, store := newTestUpdater(t, UpdateConfig{IdleEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"}, now, fake)
	if err := u.Tick(context.Background()); err == nil {
		t.Fatal("unknown mutation outcome returned success")
	}
	state, _ := store.Snapshot()
	if state.Update.Status != "unknown" || fake.updates != 1 || fake.restarts != 0 {
		t.Fatalf("unknown update was restarted: %+v updates=%d restarts=%d", state.Update, fake.updates, fake.restarts)
	}
	if err := u.Tick(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if fake.updates != 1 || fake.restarts != 0 {
		t.Fatal("unknown update outcome was replayed")
	}
}

func TestMergeConfigValidatesUpdateWindowAndTimezone(t *testing.T) {
	previous := DefaultConfig()
	previous.BaseURL = "https://sub2api.example"
	previous.AdminAPIKey = "administrator-secret"
	for _, update := range []UpdateConfig{
		{ScheduledEnabled: true, WindowStart: "03:00", WindowEnd: "02:00", Timezone: "Asia/Shanghai"},
		{ScheduledEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "Invalid/Timezone"},
		{ScheduledEnabled: true, WindowStart: "2:00", WindowEnd: "03:00", Timezone: "Asia/Shanghai"},
	} {
		input := ConfigInput{Config: previous}
		input.Update = &update
		if _, err := mergeConfig(input, previous); err == nil {
			t.Fatalf("invalid update settings accepted: %+v", update)
		}
	}
}

func TestMergeConfigExplicitEmptyUpdateDisablesPreviousModes(t *testing.T) {
	previous := DefaultConfig()
	previous.BaseURL = "https://sub2api.example"
	previous.AdminAPIKey = "administrator-secret"
	previous.Update.IdleEnabled = true
	previous.Update.ScheduledEnabled = true
	input := ConfigInput{Config: previous, Update: &UpdateConfig{}}
	merged, err := mergeConfig(input, previous)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Update.IdleEnabled || merged.Update.ScheduledEnabled || merged.Update.WindowStart != "02:00" {
		t.Fatalf("explicit empty update did not disable modes: %+v", merged.Update)
	}
}

func TestConfigInputJSONCanDisableAutomaticUpdates(t *testing.T) {
	previous := DefaultConfig()
	previous.BaseURL = "https://sub2api.example"
	previous.AdminAPIKey = "administrator-secret"
	previous.Update.IdleEnabled = true
	previous.Update.ScheduledEnabled = true
	var input ConfigInput
	if err := json.Unmarshal([]byte(`{"base_url":"https://sub2api.example","poll_interval_seconds":60,"confirm_delay_seconds":5,"concurrency":3,"update":{"idle_enabled":false,"scheduled_enabled":false},"telegram":{},"email":{}}`), &input); err != nil {
		t.Fatal(err)
	}
	merged, err := mergeConfig(input, previous)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Update.IdleEnabled || merged.Update.ScheduledEnabled {
		t.Fatalf("JSON config update did not disable both modes: %+v", merged.Update)
	}
}

func TestMergeConfigDisablesAutomaticUpdatesOnSourceChange(t *testing.T) {
	previous := DefaultConfig()
	previous.BaseURL = "https://old.example"
	previous.AdminAPIKey = "administrator-secret"
	previous.Update.IdleEnabled = true
	previous.Update.ScheduledEnabled = true
	input := ConfigInput{Config: previous}
	input.BaseURL = "https://new.example"
	merged, err := mergeConfig(input, previous)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Update.IdleEnabled || merged.Update.ScheduledEnabled {
		t.Fatal("source change kept automatic updates enabled")
	}
}

func TestVersionAtLeastRequiresTargetRelease(t *testing.T) {
	for _, tt := range []struct {
		actual, target string
		want           bool
	}{
		{"0.2.10", "0.2.10", true},
		{"0.2.11", "0.2.10", true},
		{"0.2.9", "0.2.10", false},
		{"v0.2.10", "0.2.10", true},
		{"0.2.10-beta", "0.2.10", false},
		{"dev", "0.2.10", false},
	} {
		if got := versionAtLeast(tt.actual, tt.target); got != tt.want {
			t.Errorf("versionAtLeast(%q, %q) = %t, want %t", tt.actual, tt.target, got, tt.want)
		}
	}
}

func TestUnknownUpdateRequiresAuthenticatedAcknowledgement(t *testing.T) {
	store, err := OpenStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Update(func(state *State) error {
		state.Update.Status = "unknown"
		state.Update.HasUpdate = true
		state.Update.LastAttemptDate = "2026-09-29"
		state.Update.LastAttemptVersion = "1.2.4"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(context.Background(), store, NewEngine(store, nil), nil, "admin", "panel-password")
	handler := server.Handler()
	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/update/acknowledge", nil)
	request.Header.Set("X-Quota-Watch", "1")
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized acknowledgement accepted: %d", unauthorized.Code)
	}
	_, cookie := loginForTest(t, handler, nil)
	request = httptest.NewRequest(http.MethodPost, "/api/update/acknowledge", nil)
	request.Header.Set("X-Quota-Watch", "1")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("acknowledgement failed: %d %s", response.Code, response.Body.String())
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.Update.Status != "available" || state.Update.HasUpdate || state.Update.LastAttemptDate != "" || state.Update.LastAttemptVersion != "" {
		t.Fatalf("acknowledgement did not clear the automatic retry block: %+v", state.Update)
	}
}

func TestUpdateWindowSkipsNonexistentDSTHour(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	beforeJump := time.Date(2026, 3, 8, 1, 30, 0, 0, location)
	inside, _, _, err := updateWindow(beforeJump.UTC(), UpdateConfig{
		ScheduledEnabled: true, WindowStart: "02:00", WindowEnd: "03:00", Timezone: "America/New_York",
	})
	if err != nil || inside {
		t.Fatalf("nonexistent DST window became active: inside=%t error=%v", inside, err)
	}
}
