package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScheduledForceUpdateModes(t *testing.T) {
	inside := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC) // Oct 2 02:00 Shanghai
	for _, tt := range []struct {
		name                    string
		forced, scheduled, idle bool
		offset                  time.Duration
		quiet, usageError       bool
		wantUpdates, wantReads  int
		wantTrigger             string
	}{
		{name: "forced with traffic", forced: true, scheduled: true, wantUpdates: 1, wantTrigger: "scheduled_force"},
		{name: "forced with idle also enabled", forced: true, scheduled: true, idle: true, wantUpdates: 1, wantTrigger: "scheduled_force"},
		{name: "forced skips unavailable usage records", forced: true, scheduled: true, usageError: true, wantUpdates: 1, wantTrigger: "scheduled_force"},
		{name: "forced before window", forced: true, scheduled: true, offset: -time.Minute},
		{name: "forced at window end", forced: true, scheduled: true, offset: 5 * time.Hour},
		{name: "forced before window end", forced: true, scheduled: true, offset: 5*time.Hour - time.Second, wantUpdates: 1, wantTrigger: "scheduled_force"},
		{name: "force disabled still waits for idle", scheduled: true, wantReads: 1},
		{name: "force without schedule does nothing", forced: true},
		{name: "force without schedule cannot bypass idle", forced: true, idle: true, wantReads: 1},
		{name: "force outside window cannot bypass idle", forced: true, scheduled: true, idle: true, offset: 5 * time.Hour, wantReads: 1},
		{name: "outside window retains normal idle mode", forced: true, scheduled: true, idle: true, offset: 5 * time.Hour, quiet: true, wantUpdates: 1, wantReads: 2, wantTrigger: "idle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := inside.Add(tt.offset)
			settings := defaultUpdateConfig()
			settings.WindowEnd = "07:00"
			settings.ScheduledEnabled, settings.ScheduledForceEnabled, settings.IdleEnabled = tt.scheduled, tt.forced, tt.idle
			fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now, usageExists: true, updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3"}
			if tt.quiet {
				fake.usageAt = now.Add(-11 * time.Minute)
			}
			if tt.usageError {
				fake.usageErr = errors.New("usage service unavailable")
			}
			u, store := newTestUpdater(t, settings, now, fake)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if fake.updates != tt.wantUpdates || fake.usageReads != tt.wantReads || state.Update.LastTrigger != tt.wantTrigger {
				t.Fatalf("calls updates=%d reads=%d state=%+v", fake.updates, fake.usageReads, state.Update)
			}
			if tt.wantUpdates == 1 && (fake.restarts != 1 || state.Update.Status != "success") {
				t.Fatalf("restart/verify skipped: %+v", state.Update)
			}
		})
	}
}

type forceCheckHookAPI struct {
	*fakeUpdateAPI
	afterCheck func(int)
}

func (f *forceCheckHookAPI) CheckUpdates(ctx context.Context) (SystemUpdateInfo, error) {
	info, err := f.fakeUpdateAPI.CheckUpdates(ctx)
	if f.afterCheck != nil {
		f.afterCheck(f.checks)
	}
	return info, err
}

func TestScheduledForcePreservesApprovalAndPreflight(t *testing.T) {
	for _, mode := range []string{"pending", "declined", "approved", "new release", "check error", "disable force", "close window", "close window with idle"} {
		t.Run(mode, func(t *testing.T) {
			clock := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
			settings := defaultUpdateConfig()
			settings.WindowEnd = "07:00"
			settings.ScheduledEnabled, settings.ScheduledForceEnabled = true, true
			settings.IdleEnabled = mode == "close window with idle"
			fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: clock, usageExists: true, updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3"}
			u, store := newApprovalTestUpdater(t, settings, clock, fake)
			u.now = func() time.Time { return clock }
			hook := &forceCheckHookAPI{fakeUpdateAPI: fake}
			u.newClient = func(Config) (updateAPI, error) { return hook, nil }
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fake.updates != 0 || fake.usageReads != 0 {
				t.Fatal("force bypassed initial approval")
			}
			if mode == "declined" {
				if err := store.Update(func(s *State) error { s.Update.DeclinedVersions = []string{fake.info.LatestVersion}; return nil }); err != nil {
					t.Fatal(err)
				}
			} else if mode != "pending" {
				setApprovalDecisionForTest(t, store, "approved")
			}
			switch mode {
			case "new release":
				fake.info.LatestVersion = "1.2.5"
			case "check error":
				fake.info.Warning = "release service unavailable"
			case "disable force":
				hook.afterCheck = func(checks int) {
					if checks == 2 {
						cfg, _ := store.Config()
						cfg.Update.ScheduledForceEnabled = false
						if err := store.SaveConfig(cfg); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "close window", "close window with idle":
				hook.afterCheck = func(checks int) {
					if checks == 2 {
						clock = clock.Add(5 * time.Hour)
					}
				}
			}
			err := u.Tick(context.Background())
			if (err != nil) != (mode == "check error") {
				t.Fatalf("tick: %v", err)
			}
			state, _ := store.Snapshot()
			want := 0
			if mode == "approved" {
				want = 1
			}
			if fake.updates != want || fake.usageReads != 0 {
				t.Fatalf("updates=%d reads=%d state=%+v", fake.updates, fake.usageReads, state.Update)
			}
			if mode == "approved" && (fake.checks != 2 || state.Update.LastTrigger != "scheduled_force") {
				t.Fatalf("missing forced preflight/trigger: %+v", state.Update)
			}
			if mode == "new release" && (state.UpdateApproval.Status != "pending" || state.UpdateApproval.Version != "1.2.5") {
				t.Fatalf("new version not gated: %+v", state.UpdateApproval)
			}
			if strings.HasPrefix(mode, "close window") && (!state.Update.LastAttemptAt.IsZero() || state.Update.OperationID != "") {
				t.Fatalf("late attempt was not restored: %+v", state.Update)
			}
		})
	}
}

func TestScheduledForcePreservesExecutionAndFailureGuards(t *testing.T) {
	for _, mode := range []string{"engine busy", "failed", "unknown", "no update"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
			settings := defaultUpdateConfig()
			settings.ScheduledEnabled, settings.ScheduledForceEnabled = true, true
			fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now, usageExists: true, version: "1.2.3"}
			if mode == "failed" {
				fake.updateErr = errors.New("update endpoint failed")
			}
			if mode == "no update" {
				fake.info.HasUpdate = false
			}
			u, store := newTestUpdater(t, settings, now, fake)
			if mode == "engine busy" {
				u.engine.busy.Store(true)
			}
			err := u.Tick(context.Background())
			if (err != nil) != (mode == "failed" || mode == "unknown") {
				t.Fatalf("initial tick: %v", err)
			}
			_ = u.Tick(context.Background())
			state, _ := store.Snapshot()
			want := 0
			if mode == "failed" || mode == "unknown" {
				want = 1
			}
			if fake.updates != want || fake.usageReads != 0 {
				t.Fatalf("guard bypassed: calls=%d/%d state=%+v", fake.updates, fake.usageReads, state.Update)
			}
			if mode == "engine busy" && state.Update.Status != "waiting_engine" {
				t.Fatalf("wrong busy state: %+v", state.Update)
			}
		})
	}
}

func TestScheduledForceNotificationAndConfig(t *testing.T) {
	now := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	settings := defaultUpdateConfig()
	if settings.ScheduledForceEnabled {
		t.Fatal("force must default off")
	}
	settings.ScheduledEnabled, settings.ScheduledForceEnabled, settings.NotifyWindowMissedTelegramEnabled = true, true, true
	fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now, usageExists: true}
	u, store := newTestUpdater(t, settings, now, fake)
	cfg, _ := store.Config()
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "123"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	u.engine.busy.Store(true)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	u.now = func() time.Time { return now.Add(time.Hour) }
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if len(state.Deliveries) != 1 || !strings.Contains(state.Deliveries[0].Message, "强制更新") || strings.Contains(state.Deliveries[0].Message, "不会强制更新") {
		t.Fatalf("misleading forced-window message: %+v", state.Deliveries)
	}
	message := updateSuccessMessage("1.2.3", "1.2.4", "scheduled_force", now, settings.Timezone)
	if !strings.Contains(message, "触发方式：指定时段强制更新") {
		t.Fatal(message)
	}
	// An omitted update block retains the new preference; a new source disables it.
	merged, err := mergeConfig(ConfigInput{Config: cfg}, cfg)
	if err != nil || !merged.Update.ScheduledForceEnabled {
		t.Fatalf("force preference lost: %+v %v", merged.Update, err)
	}
	input := cfg
	input.BaseURL = "https://another.example"
	merged, err = mergeConfig(ConfigInput{Config: input, Update: &input.Update}, cfg)
	if err != nil || merged.Update.ScheduledForceEnabled {
		t.Fatalf("new source retained force: %+v %v", merged.Update, err)
	}
	settings.ScheduledForceEnabled = false
	if sameUpdateTriggerSettings(settings, cfg.Update) {
		t.Fatal("force toggle not treated as trigger configuration change")
	}
}
