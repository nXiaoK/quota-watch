package main

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func newApprovalTestUpdater(t *testing.T, settings UpdateConfig, now time.Time, fake *fakeUpdateAPI) (*Updater, *Store) {
	t.Helper()
	settings.NotifyAvailableTelegramEnabled = true
	u, store := newTestUpdater(t, settings, now, fake)
	cfg, err := store.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "42"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return u, store
}

func forceApprovalVersionCheck(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	if err := store.Update(func(state *State) error {
		state.Update.LastCheckAt = now.Add(-7 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func setApprovalDecisionForTest(t *testing.T, store *Store, status string) {
	t.Helper()
	if err := store.Update(func(state *State) error {
		state.UpdateApproval.Status = status
		state.UpdateApproval.DecisionAt = state.Update.LastCheckAt
		if status == "approved" {
			state.UpdateApproval.ApprovedBy = 42
		} else {
			state.UpdateApproval.ApprovedBy = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateApprovalFirstNoticeIsOpaqueAndRefreshKeepsDecision(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{info: testUpdateInfo(), usageAt: now, usageExists: true, version: "1.2.3"}
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
	approval := state.UpdateApproval
	decoded, decodeErr := hex.DecodeString(approval.ID)
	if approval.Version != "1.2.4" || approval.Status != "pending" || len(approval.ID) != 32 || len(decoded) != 16 || decodeErr != nil || !approval.ExpiresAt.After(now) {
		t.Fatalf("first release did not create an opaque pending decision: %+v", approval)
	}
	if fake.updates != 0 || fake.usageReads != 0 {
		t.Fatalf("unapproved release reached the update path: updates=%d usage=%d", fake.updates, fake.usageReads)
	}
	if len(state.Deliveries) != 1 || state.Deliveries[0].Kind != "update_available" || state.Deliveries[0].Channel != "telegram" ||
		state.Deliveries[0].UpdateRequestID != approval.ID || !strings.Contains(state.Deliveries[0].Message, "https://github.com/Wei-Shaw/sub2api/releases/tag/v1.2.4") {
		t.Fatalf("first release notice or release link is missing: %+v", state.Deliveries)
	}

	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.UpdateApproval.ID != approval.ID || len(state.Deliveries) != 1 {
		t.Fatalf("same release generated another notice: approval=%+v deliveries=%+v", state.UpdateApproval, state.Deliveries)
	}

	for _, decision := range []string{"approved", "declined"} {
		setApprovalDecisionForTest(t, store, decision)
		forceApprovalVersionCheck(t, store, now)
		if err := u.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, err = store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if state.UpdateApproval.ID != approval.ID || state.UpdateApproval.Status != decision || len(state.Deliveries) != 1 || fake.updates != 0 {
			t.Fatalf("refresh lost %s decision or duplicated notice: approval=%+v deliveries=%+v updates=%d", decision, state.UpdateApproval, state.Deliveries, fake.updates)
		}
	}

	fake.info.LatestVersion = "1.2.5"
	forceApprovalVersionCheck(t, store, now)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.UpdateApproval.Version != "1.2.5" || state.UpdateApproval.Status != "pending" || state.UpdateApproval.ID == approval.ID || len(state.Deliveries) != 2 || fake.updates != 0 {
		t.Fatalf("new release reused an old decision: approval=%+v deliveries=%+v updates=%d", state.UpdateApproval, state.Deliveries, fake.updates)
	}
	if state.Deliveries[1].UpdateRequestID != state.UpdateApproval.ID || !strings.Contains(state.Deliveries[1].Message, "/tag/v1.2.5") {
		t.Fatalf("new release notice is missing: %+v", state.Deliveries[1])
	}
}

func TestUpdateApprovalGatesIdleAndScheduledUpdates(t *testing.T) {
	now := time.Date(2026, 9, 28, 18, 5, 0, 0, time.UTC) // 02:05 in Asia/Shanghai
	for _, mode := range []string{"idle", "scheduled"} {
		t.Run(mode, func(t *testing.T) {
			fake := &fakeUpdateAPI{
				info: testUpdateInfo(), usageAt: now.Add(-11 * time.Minute), usageExists: true,
				updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
			}
			settings := defaultUpdateConfig()
			settings.IdleEnabled = mode == "idle"
			settings.ScheduledEnabled = mode == "scheduled"
			u, store := newApprovalTestUpdater(t, settings, now, fake)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fake.updates != 0 || fake.restarts != 0 || fake.usageReads != 0 {
				t.Fatalf("%s mode updated before approval: updates=%d restarts=%d usage=%d", mode, fake.updates, fake.restarts, fake.usageReads)
			}
			setApprovalDecisionForTest(t, store, "approved")
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if fake.updates != 1 || fake.restarts != 1 || fake.usageReads != 3 || state.Update.Status != "success" || state.Update.CurrentVersion != "1.2.4" {
				t.Fatalf("approved %s release was not updated once after three idle checks: updates=%d restarts=%d usage=%d state=%+v", mode, fake.updates, fake.restarts, fake.usageReads, state.Update)
			}
		})
	}
}

func TestUpdateApprovalDeclineAndCheckFailureDoNotInstall(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for _, outcome := range []string{"declined", "check_failed"} {
		t.Run(outcome, func(t *testing.T) {
			fake := &fakeUpdateAPI{
				info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true,
				updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
			}
			settings := defaultUpdateConfig()
			settings.IdleEnabled = true
			u, store := newApprovalTestUpdater(t, settings, now, fake)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if outcome == "declined" {
				setApprovalDecisionForTest(t, store, "declined")
			} else {
				setApprovalDecisionForTest(t, store, "approved")
				fake.info.Warning = "GitHub release check failed"
				forceApprovalVersionCheck(t, store, now)
			}
			err := u.Tick(context.Background())
			if outcome == "check_failed" && err == nil {
				t.Fatal("release check failure was ignored")
			}
			if outcome == "declined" && err != nil {
				t.Fatal(err)
			}
			state, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if fake.updates != 0 || fake.restarts != 0 || fake.usageReads != 0 {
				t.Fatalf("%s release reached the update path: updates=%d restarts=%d usage=%d state=%+v", outcome, fake.updates, fake.restarts, fake.usageReads, state.Update)
			}
		})
	}
}

func TestUpdateApprovalIsRecheckedImmediatelyBeforeMutation(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for _, change := range []string{"decision_revoked", "new_release"} {
		t.Run(change, func(t *testing.T) {
			fake := &fakeUpdateAPI{
				info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true,
				updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
			}
			settings := defaultUpdateConfig()
			settings.IdleEnabled = true
			u, store := newApprovalTestUpdater(t, settings, now, fake)
			if err := u.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			setApprovalDecisionForTest(t, store, "approved")
			fake.afterUsage = func(read int) {
				if read != 2 {
					return
				}
				if change == "decision_revoked" {
					setApprovalDecisionForTest(t, store, "declined")
				} else {
					fake.info.LatestVersion = "1.2.5"
				}
			}
			// The second usage read happens inside perform, after Tick's first
			// approval decision but before the mutation request is sent.
			_ = u.Tick(context.Background())
			if fake.usageReads != 2 || fake.updates != 0 || fake.restarts != 0 {
				t.Fatalf("%s passed the final approval/version gate: usage=%d updates=%d restarts=%d", change, fake.usageReads, fake.updates, fake.restarts)
			}
		})
	}
}

func TestDeclinedUpdateStaysBlockedAfterConfirmationSwitchIsDisabled(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{
		info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true,
		updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
	}
	settings := defaultUpdateConfig()
	settings.IdleEnabled = true
	u, store := newApprovalTestUpdater(t, settings, now, fake)
	u.engine.Now = func() time.Time { return now }
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := u.engine.DecideUpdateVersion(context.Background(), TelegramCallback{
		FromID: 42, ChatID: 42, ChatType: "private", Data: "qw:ur:" + state.UpdateApproval.ID,
	})
	if err != nil || decision.RequestID != state.UpdateApproval.ID {
		t.Fatalf("Telegram decline was not stored: decision=%+v error=%v", decision, err)
	}
	state, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.UpdateApproval.Status != "declined" || !containsString(state.Update.DeclinedVersions, "1.2.4") {
		t.Fatalf("declined version was not recorded: approval=%+v update=%+v", state.UpdateApproval, state.Update)
	}
	cfg, err := store.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update.NotifyAvailableTelegramEnabled = false
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.updates != 0 || fake.restarts != 0 || fake.usageReads != 0 || state.Update.Status != "declined" ||
		!containsString(state.Update.DeclinedVersions, "1.2.4") {
		t.Fatalf("disabling confirmation bypassed a declined version: updates=%d restarts=%d usage=%d state=%+v", fake.updates, fake.restarts, fake.usageReads, state.Update)
	}
}

func TestUpdateApprovalChecksUsageAfterVersionPreflight(t *testing.T) {
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	fake := &fakeUpdateAPI{
		info: testUpdateInfo(), usageAt: now.Add(-time.Hour), usageExists: true,
		updateResult: SystemUpdateResult{NeedRestart: true}, version: "1.2.3",
	}
	settings := defaultUpdateConfig()
	settings.IdleEnabled = true
	u, store := newApprovalTestUpdater(t, settings, now, fake)
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	setApprovalDecisionForTest(t, store, "approved")
	fake.afterUsage = func(read int) {
		if read == 3 {
			fake.usageAt = now.Add(-time.Minute)
		}
	}
	if err := u.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fake.usageReads != 3 || fake.checks != 2 || fake.updates != 0 || fake.restarts != 0 ||
		state.Update.Status != "waiting_idle" || !state.Update.LastAttemptAt.IsZero() {
		t.Fatalf("new activity after version preflight did not stop update: checks=%d usage=%d updates=%d restarts=%d state=%+v",
			fake.checks, fake.usageReads, fake.updates, fake.restarts, state.Update)
	}
}
