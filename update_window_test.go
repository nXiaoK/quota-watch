package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScheduledWindowMissedNotificationReasons(t *testing.T) {
	start := time.Date(2026, 10, 1, 18, 5, 0, 0, time.UTC) // Oct 2 02:05 Shanghai
	end := time.Date(2026, 10, 1, 23, 0, 0, 0, time.UTC)   // Oct 2 07:00 Shanghai
	for _, tt := range []struct {
		name, reason string
		setup        func(*fakeUpdateAPI, *Config, *Updater, *Store)
		wantError    bool
		wantReport   bool
	}{
		{name: "traffic", reason: "连续 10 分钟无使用记录", wantReport: true},
		{name: "usage read failed", reason: "读取使用记录失败", wantError: true, wantReport: true,
			setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, _ *Store) { f.usageErr = errors.New("usage unavailable") }},
		{name: "version check failed", reason: "版本检查失败", wantError: true, wantReport: true,
			setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, _ *Store) {
				f.info.Warning = "release service unavailable"
			}},
		{name: "pending approval", reason: "仍待 Telegram 确认", wantReport: true,
			setup: func(_ *fakeUpdateAPI, c *Config, _ *Updater, _ *Store) {
				c.Update.NotifyAvailableTelegramEnabled = true
			}},
		{name: "engine busy", reason: "未取得更新执行锁", wantReport: true,
			setup: func(f *fakeUpdateAPI, _ *Config, u *Updater, _ *Store) {
				f.usageAt = start.Add(-11 * time.Minute)
				u.engine.busy.Store(true)
			}},
		{name: "attempt failed", reason: "更新尝试失败", wantError: true, wantReport: true,
			setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, _ *Store) {
				f.usageAt = start.Add(-11 * time.Minute)
				f.updateErr = errors.New("update endpoint failed")
			}},
		{name: "success", setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, _ *Store) {
			f.usageAt = start.Add(-11 * time.Minute)
			f.version = f.info.LatestVersion
			f.updateResult.NeedRestart = true
		}},
		{name: "no new version", setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, _ *Store) { f.info.HasUpdate = false }},
		{name: "declined", setup: func(f *fakeUpdateAPI, _ *Config, _ *Updater, s *Store) {
			if err := s.Update(func(state *State) error { state.Update.DeclinedVersions = []string{f.info.LatestVersion}; return nil }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "notification disabled", setup: func(_ *fakeUpdateAPI, c *Config, _ *Updater, _ *Store) {
			c.Update.NotifyWindowMissedTelegramEnabled = false
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			settings := defaultUpdateConfig()
			settings.ScheduledEnabled, settings.NotifyWindowMissedTelegramEnabled = true, true
			settings.WindowEnd = "07:00"
			f := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: start, usageExists: true}
			u, store := newTestUpdater(t, settings, start, f)
			cfg, _ := store.Config()
			cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "123"}
			if tt.setup != nil {
				tt.setup(f, &cfg, u, store)
			}
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			clock := start
			u.now = func() time.Time { return clock }
			if err := u.Tick(context.Background()); (err != nil) != tt.wantError {
				t.Fatalf("initial tick: %v", err)
			}
			before, _ := store.Snapshot()
			for _, d := range before.Deliveries {
				if d.Kind == "update_window_missed" {
					t.Fatal("report before window end")
				}
			}
			clock = end
			_ = u.Tick(context.Background())
			// A restart and later version checks must not duplicate the report.
			restarted := NewUpdater(store, u.engine, nil)
			restarted.now, restarted.newClient = u.now, u.newClient
			clock = end.Add(24 * time.Hour)
			_ = restarted.Tick(context.Background())
			state, _ := store.Snapshot()
			reports := []Delivery{}
			for _, d := range state.Deliveries {
				if d.Kind == "update_window_missed" {
					reports = append(reports, d)
				}
			}
			want := 0
			if tt.wantReport {
				want = 1
			}
			if len(reports) != want {
				t.Fatalf("reports=%+v state=%+v", reports, state.Update)
			}
			if !tt.wantReport {
				return
			}
			d := reports[0]
			if d.Status != "pending" || d.Channel != "telegram" || !d.CreatedAt.Equal(end) {
				t.Fatalf("delivery: %+v", d)
			}
			for _, text := range []string{tt.reason, "2026-10-02 02:00–07:00", "Asia/Shanghai", "窗口内最后检查：2026-10-02 02:05:00", "通知生成时间：2026-10-02 07:00:00"} {
				if !strings.Contains(d.Message, text) {
					t.Errorf("message missing %q: %s", text, d.Message)
				}
			}
		})
	}
}

func TestScheduledWindowReportsApprovedVersionOnlyWithinWindow(t *testing.T) {
	clock := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC) // 01:00 Shanghai
	settings := defaultUpdateConfig()
	settings.WindowEnd = "07:00"
	settings.ScheduledEnabled, settings.NotifyWindowMissedTelegramEnabled, settings.NotifyAvailableTelegramEnabled = true, true, true
	f := &fakeUpdateAPI{info: testUpdateInfo(), usageExists: true}
	u, store := newTestUpdater(t, settings, clock, f)
	cfg, _ := store.Config()
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "123"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	u.now = func() time.Time { return clock }
	u.engine.Now = u.now
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	decision, err := u.engine.DecideUpdateVersion(context.Background(), TelegramCallback{Data: "qw:ua:" + state.UpdateApproval.ID, ChatID: 123, FromID: 123, ChatType: "private"})
	if err != nil || !strings.Contains(decision.Text, "已加入") {
		t.Fatalf("approve: %+v %v", decision, err)
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.updates != 0 || f.usageReads != 0 {
		t.Fatal("approval triggered update outside window")
	}
	clock = clock.Add(time.Hour)
	f.usageAt = clock
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Hour)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Snapshot()
	if f.updates != 0 {
		t.Fatal("busy window forced an update")
	}
	count := 0
	for _, d := range state.Deliveries {
		if d.Kind == "update_window_missed" {
			count++
			if !strings.Contains(d.Message, "空闲条件") {
				t.Fatal(d.Message)
			}
		}
	}
	if count != 1 {
		t.Fatalf("report count %d", count)
	}
	// The approval survives the missed window and can update the next day.
	clock = clock.Add(19 * time.Hour)
	f.usageAt = clock.Add(-11 * time.Minute)
	f.version = f.info.LatestVersion
	f.updateResult.NeedRestart = true
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.updates != 1 {
		t.Fatalf("next window did not update: %d", f.updates)
	}
}

func TestScheduledWindowNotificationDeliveryRetryAndDisable(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "disable"}[disabled], func(t *testing.T) {
			clock := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
			settings := defaultUpdateConfig()
			settings.ScheduledEnabled, settings.NotifyWindowMissedTelegramEnabled = true, true
			f := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: clock, usageExists: true}
			u, store := newTestUpdater(t, settings, clock, f)
			cfg, _ := store.Config()
			cfg.Telegram.Enabled = true
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			u.now = func() time.Time { return clock }
			u.engine.Now = u.now
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(time.Hour)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if disabled {
				cfg.Update.NotifyWindowMissedTelegramEnabled = false
				if err := store.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			u.engine.Notify = func(_ context.Context, _ Config, channel, message string) error {
				calls++
				if channel != "telegram" || !strings.Contains(message, "指定时段未完成更新") {
					t.Fatal("wrong delivery")
				}
				if calls == 1 {
					return errors.New("temporary send failure")
				}
				return nil
			}
			err := u.engine.processDeliveries(context.Background())
			if disabled {
				state, _ := store.Snapshot()
				if err != nil || calls != 0 || state.Deliveries[0].Status != "skipped" {
					t.Fatalf("disabled delivery: %v %d %+v", err, calls, state.Deliveries)
				}
				return
			}
			if err == nil || calls != 1 {
				t.Fatalf("first send: %v %d", err, calls)
			}
			clock = clock.Add(29 * time.Second)
			if err := u.engine.processDeliveries(context.Background()); err != nil || calls != 1 {
				t.Fatal("premature retry")
			}
			clock = clock.Add(time.Second)
			if err := u.engine.processDeliveries(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, _ := store.Snapshot()
			if calls != 2 || state.Deliveries[0].Status != "succeeded" || f.updates != 0 {
				t.Fatalf("retry changed update: %d %+v", calls, state)
			}
		})
	}
}

func TestScheduledWindowReportSurvivesDelayedTickAndDeliveryPruning(t *testing.T) {
	clock := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	settings := defaultUpdateConfig()
	settings.ScheduledEnabled, settings.NotifyWindowMissedTelegramEnabled = true, true
	f := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: clock, usageExists: true}
	u, store := newTestUpdater(t, settings, clock, f)
	cfg, _ := store.Config()
	cfg.Telegram.Enabled = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	u.now = func() time.Time { return clock }
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// No ticks until the following day, outside its scheduled window.
	clock = clock.Add(26 * time.Hour)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if len(state.Deliveries) != 1 || !strings.Contains(state.Deliveries[0].Message, "窗口内最后检查：2026-10-02 02:00:00") ||
		!strings.Contains(state.Deliveries[0].Message, "通知生成时间：2026-10-03 04:00:00") {
		t.Fatalf("delayed report: %+v", state.Deliveries)
	}
	if err := store.Update(func(s *State) error { s.Deliveries = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Snapshot()
	if len(state.Deliveries) != 0 {
		t.Fatal("pruning duplicated a reported window")
	}
}

func TestScheduledWindowSettingChangesDiscardOldWindow(t *testing.T) {
	for _, change := range []string{"disable", "window", "decline"} {
		t.Run(change, func(t *testing.T) {
			clock := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
			settings := defaultUpdateConfig()
			settings.ScheduledEnabled, settings.NotifyWindowMissedTelegramEnabled = true, true
			f := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: clock, usageExists: true}
			u, store := newTestUpdater(t, settings, clock, f)
			cfg, _ := store.Config()
			cfg.Telegram.Enabled = true
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			u.now = func() time.Time { return clock }
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "disable":
				cfg.Update.NotifyWindowMissedTelegramEnabled = false
			case "window":
				cfg.Update.WindowStart, cfg.Update.WindowEnd = "05:00", "06:00"
			case "decline":
				if err := store.Update(func(s *State) error { s.Update.DeclinedVersions = []string{f.info.LatestVersion}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(time.Hour)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if change == "disable" {
				cfg.Update.NotifyWindowMissedTelegramEnabled = true
				if err := store.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
				if err := u.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			state, _ := store.Snapshot()
			if len(state.Deliveries) != 0 {
				t.Fatalf("obsolete window notified: %+v", state.Deliveries)
			}
		})
	}
}

func TestMissedWindowNotificationConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BaseURL, cfg.AdminAPIKey = "https://sub2api.example", "test-key"
	cfg.Update.NotifyWindowMissedTelegramEnabled = true
	if _, err := mergeConfig(ConfigInput{Config: cfg, Update: &cfg.Update}, DefaultConfig()); err == nil || !strings.Contains(err.Error(), "时段未更新") {
		t.Fatalf("notification without Telegram accepted: %v", err)
	}
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "123"}
	merged, err := mergeConfig(ConfigInput{Config: cfg, Update: &cfg.Update}, DefaultConfig())
	if err != nil || !merged.Update.NotifyWindowMissedTelegramEnabled {
		t.Fatalf("valid setting: %+v %v", merged.Update, err)
	}
	previous := merged
	cfg.BaseURL = "https://another.example"
	merged, err = mergeConfig(ConfigInput{Config: cfg, Update: &cfg.Update}, previous)
	if err != nil || merged.Update.NotifyWindowMissedTelegramEnabled {
		t.Fatalf("new source retained notification: %+v %v", merged.Update, err)
	}
}
