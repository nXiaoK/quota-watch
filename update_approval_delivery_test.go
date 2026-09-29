package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func queuedUpdateNotice(t *testing.T, now time.Time) (*Updater, *Store, *fakeUpdateAPI, string) {
	t.Helper()
	fake := &fakeUpdateAPI{info: testUpdateInfo(), version: "1.2.3"}
	settings := defaultUpdateConfig()
	settings.IdleEnabled = true
	u, store := newApprovalTestUpdater(t, settings, now, fake)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Deliveries) != 1 || state.UpdateApproval.Status != "pending" {
		t.Fatalf("missing queued release notice: %+v", state)
	}
	u.engine.Now = func() time.Time { return now }
	return u, store, fake, state.UpdateApproval.ID
}

func TestUpdateAvailableDeliveryDispatchesVersionAndOpaqueID(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	u, store, _, id := queuedUpdateNotice(t, now)
	calls := 0
	u.engine.NotifyUpdateAvailable = func(_ context.Context, cfg Config, version, requestID string) error {
		calls++
		if !cfg.Telegram.Enabled || cfg.Telegram.ChatID != "42" || version != "1.2.4" || requestID != id {
			t.Fatalf("wrong release notification arguments: cfg=%+v version=%q id=%q", cfg.Telegram, version, requestID)
		}
		return nil
	}
	if err := u.engine.processDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || state.Deliveries[0].Status != "succeeded" || state.Deliveries[0].Attempts != 1 {
		t.Fatalf("release notice was not dispatched once: calls=%d delivery=%+v", calls, state.Deliveries[0])
	}
	if err := u.engine.processDeliveries(context.Background()); err != nil || calls != 1 {
		t.Fatalf("succeeded release notice was sent again: err=%v calls=%d", err, calls)
	}
}

func TestUpdateAvailableDeliveryFailurePersistsAndRetries(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	u, store, _, _ := queuedUpdateNotice(t, now)
	clock := now
	u.engine.Now = func() time.Time { return clock }
	calls := 0
	u.engine.NotifyUpdateAvailable = func(context.Context, Config, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("temporary Telegram failure")
		}
		return nil
	}
	if err := u.engine.processDeliveries(context.Background()); err == nil {
		t.Fatal("failed release notification returned success")
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || state.Deliveries[0].Status != "failed" || state.Deliveries[0].Attempts != 1 || !state.Deliveries[0].NextAttemptAt.After(now) {
		t.Fatalf("failed release notice was not persisted for retry: calls=%d delivery=%+v", calls, state.Deliveries[0])
	}
	clock = state.Deliveries[0].NextAttemptAt.Add(-time.Second)
	if err := u.engine.processDeliveries(context.Background()); err != nil || calls != 1 {
		t.Fatalf("release notice retried early: err=%v calls=%d", err, calls)
	}
	clock = state.Deliveries[0].NextAttemptAt
	if err := u.engine.processDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || state.Deliveries[0].Status != "succeeded" || state.Deliveries[0].Attempts != 2 {
		t.Fatalf("release notice did not succeed on retry: calls=%d delivery=%+v", calls, state.Deliveries[0])
	}
}

func TestUpdateAvailableDeliverySkipsHandledOrReplacedRelease(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for _, change := range []string{"approved", "declined", "new_release"} {
		t.Run(change, func(t *testing.T) {
			u, store, fake, oldID := queuedUpdateNotice(t, now)
			if change == "new_release" {
				fake.info.LatestVersion = "1.2.5"
				forceApprovalVersionCheck(t, store, now)
				if err := u.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				setApprovalDecisionForTest(t, store, change)
			}
			calls := 0
			u.engine.NotifyUpdateAvailable = func(_ context.Context, _ Config, version, requestID string) error {
				calls++
				if change != "new_release" || version != "1.2.5" || requestID == oldID {
					t.Fatalf("stale release notice was dispatched: change=%s version=%q id=%q", change, version, requestID)
				}
				return nil
			}
			if err := u.engine.processDeliveries(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if change == "new_release" {
				if calls != 1 || len(state.Deliveries) != 2 || state.Deliveries[0].Status != "cancelled" || state.Deliveries[1].Status != "succeeded" {
					t.Fatalf("replaced release was sent or new release was lost: calls=%d deliveries=%+v", calls, state.Deliveries)
				}
			} else if calls != 0 || state.Deliveries[0].Status != "skipped" {
				t.Fatalf("handled release notice was sent: change=%s calls=%d delivery=%+v", change, calls, state.Deliveries[0])
			}
		})
	}
}

func TestUpdateAvailableDeliveryStopsWhenNotificationSwitchTurnsOff(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	u, store, _, _ := queuedUpdateNotice(t, now)
	cfg, err := store.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update.NotifyAvailableTelegramEnabled = false
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	calls := 0
	u.engine.NotifyUpdateAvailable = func(context.Context, Config, string, string) error {
		calls++
		return nil
	}
	if err := u.engine.processDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(state.Deliveries) != 1 || state.Deliveries[0].Status != "cancelled" || state.UpdateApproval.ID != "" {
		t.Fatalf("disabled notification switch left a sendable approval: calls=%d approval=%+v deliveries=%+v", calls, state.UpdateApproval, state.Deliveries)
	}
}

func TestUpdateAvailableDeliveryCancellationSurvivesInFlightSendResult(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for _, change := range []string{"switch_off", "chat_changed"} {
		for _, outcome := range []string{"send_succeeded", "send_failed"} {
			t.Run(change+"/"+outcome, func(t *testing.T) {
				u, store, _, _ := queuedUpdateNotice(t, now)
				calls := 0
				sendFailure := errors.New("Telegram response was lost")
				u.engine.NotifyUpdateAvailable = func(context.Context, Config, string, string) error {
					calls++
					cfg, err := store.Config()
					if err != nil {
						return err
					}
					if change == "switch_off" {
						cfg.Update.NotifyAvailableTelegramEnabled = false
					} else {
						cfg.Telegram.ChatID = "43"
					}
					if err := store.SaveConfig(cfg); err != nil {
						return err
					}
					if outcome == "send_failed" {
						return sendFailure
					}
					return nil
				}
				err := u.engine.processDeliveries(context.Background())
				if outcome == "send_failed" && !errors.Is(err, sendFailure) || outcome == "send_succeeded" && err != nil {
					t.Fatalf("unexpected send result: %v", err)
				}
				state, err := store.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || state.Deliveries[0].Status != "cancelled" || state.Deliveries[0].Attempts != 1 || state.UpdateApproval.ID != "" {
					t.Fatalf("%s overwrote a cancelled in-flight notice after %s: calls=%d approval=%+v delivery=%+v",
						outcome, change, calls, state.UpdateApproval, state.Deliveries[0])
				}
				if err := u.engine.processDeliveries(context.Background()); err != nil || calls != 1 {
					t.Fatalf("cancelled notice was retried: err=%v calls=%d", err, calls)
				}
			})
		}
	}
}
