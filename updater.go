package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	updateTickInterval  = time.Minute
	updateCheckInterval = 6 * time.Hour
	updateRetryInterval = 15 * time.Minute
	updateIdlePeriod    = 10 * time.Minute
	updateVerifyTimeout = 2 * time.Minute
)

var errUpdateApprovalChanged = errors.New("更新版本确认或配置已改变")

type updateAPI interface {
	LatestUsage(context.Context) (time.Time, bool, error)
	CheckUpdates(context.Context) (SystemUpdateInfo, error)
	PerformSystemUpdate(context.Context, string) (SystemUpdateResult, error)
	RestartSystem(context.Context, string) error
	RunningVersion(context.Context) (string, error)
}

// Updater runs independently of quota sampling. Its long update request holds
// the engine's busy flag so quota resets cannot race with a service restart.
type Updater struct {
	store     *Store
	engine    *Engine
	logger    *slog.Logger
	now       func() time.Time
	newClient func(Config) (updateAPI, error)
}

func NewUpdater(store *Store, engine *Engine, logger *slog.Logger) *Updater {
	if logger == nil {
		logger = slog.Default()
	}
	return &Updater{
		store: store, engine: engine, logger: logger, now: time.Now,
		newClient: func(cfg Config) (updateAPI, error) {
			client, err := NewAdminClient(cfg)
			if err == nil {
				client.logger = logger
				client.logEnabled = store.VerboseLoggingEnabled
				client.logSource = "auto_update"
			}
			return client, err
		},
	}
}

func (u *Updater) Run(ctx context.Context) {
	ticker := time.NewTicker(updateTickInterval)
	defer ticker.Stop()
	for {
		if err := u.Tick(ctx); err != nil && ctx.Err() == nil {
			u.logger.Warn("自动更新检查未完成", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (u *Updater) Tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := u.store.Config()
	if err != nil {
		return err
	}
	if !cfg.Update.IdleEnabled && !cfg.Update.ScheduledEnabled && !cfg.Update.NotifyAvailableTelegramEnabled {
		state, err := u.store.Snapshot()
		if err != nil {
			return err
		}
		if state.Update.Status == "updating" || state.Update.Status == "restarting" || state.Update.Status == "unknown" {
			client, err := u.newClient(cfg)
			if err != nil {
				return err
			}
			return u.reconcile(ctx, cfg, client, state.Update, u.now().UTC())
		}
		if state.Update.Status != "" {
			return nil
		}
		return u.setUpdateState(cfg, func(state *UpdateState) {
			state.Status = "disabled"
		})
	}
	client, err := u.newClient(cfg)
	if err != nil {
		return u.recordUpdateError(cfg, "failed", err)
	}
	state, err := u.store.Snapshot()
	if err != nil {
		return err
	}
	now := u.now().UTC()
	if state.Update.Status == "updating" || state.Update.Status == "restarting" || state.Update.Status == "unknown" {
		return u.reconcile(ctx, cfg, client, state.Update, now)
	}
	inside, day, windowStart, err := updateWindow(now, cfg.Update)
	if err != nil {
		return u.recordUpdateError(cfg, "failed", err)
	}
	checkDue := state.Update.LastCheckAt.IsZero() || now.Sub(state.Update.LastCheckAt) >= updateCheckInterval ||
		(state.Update.Status == "check_failed" && now.Sub(state.Update.LastCheckAt) >= updateRetryInterval)
	if cfg.Update.NotifyAvailableTelegramEnabled && manualTelegramReady(cfg.Telegram) && state.Update.HasUpdate &&
		(!updateApprovalScopeValid(cfg, &state, state.UpdateApproval) ||
			state.UpdateApproval.Status == "pending" && !state.UpdateApproval.ExpiresAt.After(now) ||
			state.UpdateApproval.Status == "expired" || state.UpdateApproval.Status == "invalid") {
		checkDue = true
	}
	if inside && state.Update.LastScheduleCheckDate != day &&
		(state.Update.LastCheckAt.Before(windowStart) || now.Sub(state.Update.LastCheckAt) >= updateRetryInterval) {
		checkDue = true
	}
	if checkDue {
		if err := u.checkVersion(ctx, cfg, client, now, inside, day); err != nil {
			return err
		}
		state, err = u.store.Snapshot()
		if err != nil {
			return err
		}
	}
	if !state.Update.HasUpdate || state.Update.LatestVersion == "" {
		return nil
	}
	if updateVersionDeclined(&state) {
		if state.Update.Status == "declined" {
			return nil
		}
		return u.setUpdateState(cfg, func(s *UpdateState) { s.Status = "declined" })
	}
	if cfg.Update.NotifyAvailableTelegramEnabled && !updateApprovalGranted(cfg, &state) {
		status := "awaiting_approval"
		if state.UpdateApproval.Status == "declined" {
			status = "declined"
		}
		u.logDiagnostic("approval", status, "version", state.Update.LatestVersion)
		if state.Update.Status == status {
			return nil
		}
		return u.setUpdateState(cfg, func(s *UpdateState) { s.Status = status })
	}
	if !cfg.Update.IdleEnabled && !cfg.Update.ScheduledEnabled {
		return nil
	}
	if state.Update.LastAttemptVersion == state.Update.LatestVersion && state.Update.LastAttemptDate == day {
		if state.Update.Status == "failed" {
			return nil
		}
		return u.setUpdateState(cfg, func(s *UpdateState) { s.Status = "already_attempted" })
	}
	if !cfg.Update.IdleEnabled && !inside {
		return u.setUpdateState(cfg, func(s *UpdateState) { s.Status = "available" })
	}
	latest, exists, err := client.LatestUsage(ctx)
	if err != nil {
		u.logDiagnostic("idle_check", "read_failed", "step", "initial")
		return u.recordUpdateError(cfg, "waiting_idle", fmt.Errorf("读取使用记录失败: %w", err))
	}
	idle, err := u.observeIdle(cfg, now, latest, exists)
	if err != nil || !idle {
		return err
	}
	trigger := "idle"
	if inside && cfg.Update.ScheduledEnabled {
		trigger = "scheduled"
	}
	return u.perform(ctx, cfg, client, now, day, trigger)
}

func (u *Updater) checkVersion(ctx context.Context, cfg Config, client updateAPI, now time.Time, inside bool, day string) error {
	info, err := client.CheckUpdates(ctx)
	if err != nil {
		u.logDiagnostic("version_check", "failed")
		return u.recordCheckError(cfg, now, err)
	}
	if info.Warning != "" {
		u.logDiagnostic("version_check", "warning")
		return u.recordCheckError(cfg, now, errors.New(info.Warning))
	}
	if info.CurrentVersion == "" || info.LatestVersion == "" || info.BuildType != "release" {
		u.logDiagnostic("version_check", "unsupported", "build_type", info.BuildType)
		return u.recordCheckError(cfg, now, errors.New("主站未返回可自动更新的发布版本"))
	}
	if cfg.Update.NotifyAvailableTelegramEnabled && info.HasUpdate {
		if _, _, err := sub2apiReleaseURL(info.LatestVersion); err != nil {
			return u.recordCheckError(cfg, now, err)
		}
	}
	status := "up_to_date"
	if info.HasUpdate {
		status = "available"
	}
	u.logDiagnostic("version_check", status, "current_version", info.CurrentVersion, "latest_version", info.LatestVersion,
		"build_type", info.BuildType, "cached", info.Cached, "inside_window", inside)
	return u.store.UpdateForConfigWithCurrent(cfg, func(state *State, current Config) error {
		s := &state.Update
		s.LastCheckAt = now
		s.CurrentVersion = info.CurrentVersion
		s.LatestVersion = info.LatestVersion
		s.HasUpdate = info.HasUpdate
		s.EmptySince = time.Time{}
		s.LastError = ""
		if inside {
			s.LastScheduleCheckDate = day
		}
		if !info.HasUpdate {
			s.Status = "up_to_date"
			if state.UpdateApproval.ID != "" {
				state.UpdateApproval.Status = "superseded"
				cancelUpdateAvailableDeliveries(state, now)
			}
			return nil
		}
		if updateVersionDeclined(state) {
			s.Status = "declined"
			cancelUpdateAvailableDeliveries(state, now)
			return nil
		}
		if !current.Update.NotifyAvailableTelegramEnabled {
			s.Status = "available"
			return nil
		}
		if !manualTelegramReady(current.Telegram) {
			s.Status = "approval_unavailable"
			cancelUpdateAvailableDeliveries(state, now)
			state.UpdateApproval = UpdateApproval{}
			return nil
		}
		approval := state.UpdateApproval
		stillCurrent := approval.Version == info.LatestVersion && updateApprovalScopeValid(current, state, approval) &&
			(approval.Status == "approved" || approval.Status == "declined" ||
				approval.Status == "pending" && approval.ExpiresAt.After(now))
		if !stillCurrent {
			cancelUpdateAvailableDeliveries(state, now)
			var err error
			approval, err = newUpdateApproval(current, info.LatestVersion, now)
			if err != nil {
				return err
			}
			state.UpdateApproval = approval
			if err := queueUpdateAvailableDelivery(state, approval, now); err != nil {
				return err
			}
			u.logger.Info("Sub2API 发现新版本，等待 Telegram 确认", "version", info.LatestVersion)
		}
		s.Status = "awaiting_approval"
		if approval.Status == "approved" {
			s.Status = "available"
		} else if approval.Status == "declined" {
			s.Status = "declined"
		}
		return nil
	})
}

func (u *Updater) observeIdle(cfg Config, now, latest time.Time, exists bool) (bool, error) {
	state, err := u.store.Snapshot()
	if err != nil {
		return false, err
	}
	firstEmpty := state.Update.EmptySince
	if exists {
		firstEmpty = time.Time{}
	}
	if !exists && firstEmpty.IsZero() {
		firstEmpty = now
	}
	idle := exists && !latest.After(now.Add(-updateIdlePeriod)) || !exists && now.Sub(firstEmpty) >= updateIdlePeriod
	err = u.setUpdateState(cfg, func(s *UpdateState) {
		s.EmptySince = firstEmpty
		s.LastError = ""
		if !idle {
			s.Status = "waiting_idle"
		}
	})
	if err == nil {
		status := "waiting"
		if idle {
			status = "idle"
		}
		quietSince := firstEmpty
		if exists {
			quietSince = latest
		}
		quietSeconds := int64(now.Sub(quietSince).Seconds())
		if quietSeconds < 0 {
			quietSeconds = 0
		}
		u.logDiagnostic("idle_check", status, "usage_record_found", exists, "quiet_seconds", quietSeconds,
			"required_seconds", int64(updateIdlePeriod.Seconds()))
	}
	return idle, err
}

func (u *Updater) perform(ctx context.Context, cfg Config, client updateAPI, now time.Time, day, trigger string) error {
	if !u.engine.busy.CompareAndSwap(false, true) {
		u.logDiagnostic("update", "engine_busy")
		return nil
	}
	defer u.engine.busy.Store(false)
	current, err := u.store.Config()
	if err != nil {
		return err
	}
	if current.BaseURL != cfg.BaseURL || current.AdminAPIKey != cfg.AdminAPIKey || !sameUpdateTriggerSettings(current.Update, cfg.Update) {
		u.logDiagnostic("update", "config_changed")
		return nil
	}
	// A slow version or usage query can cross the end of the scheduled window.
	// When idle mode is disabled, do not begin a late update.
	if !cfg.Update.IdleEnabled {
		inside, _, _, err := updateWindow(u.now().UTC(), cfg.Update)
		if err != nil {
			return err
		}
		if !inside {
			u.logDiagnostic("update", "window_closed")
			return nil
		}
	}
	if trigger == "scheduled" && cfg.Update.IdleEnabled {
		inside, _, _, err := updateWindow(u.now().UTC(), cfg.Update)
		if err != nil {
			return err
		}
		if !inside {
			trigger = "idle"
		}
	}
	latest, exists, err := client.LatestUsage(ctx)
	if err != nil {
		u.logDiagnostic("idle_check", "read_failed", "step", "before_update")
		return u.recordUpdateError(cfg, "waiting_idle", fmt.Errorf("更新前复查使用记录失败: %w", err))
	}
	idle, err := u.observeIdle(cfg, u.now().UTC(), latest, exists)
	if err != nil || !idle {
		return err
	}
	if !cfg.Update.IdleEnabled {
		inside, _, _, err := updateWindow(u.now().UTC(), cfg.Update)
		if err != nil {
			return err
		}
		if !inside {
			u.logDiagnostic("update", "window_closed")
			return nil
		}
	} else if trigger == "scheduled" {
		inside, _, _, err := updateWindow(u.now().UTC(), cfg.Update)
		if err != nil {
			return err
		}
		if !inside {
			trigger = "idle"
		}
	}
	state, err := u.store.Snapshot()
	if err != nil {
		return err
	}
	if !state.Update.HasUpdate || state.Update.LatestVersion == "" {
		return nil
	}
	if updateVersionDeclined(&state) {
		return nil
	}
	if cfg.Update.NotifyAvailableTelegramEnabled {
		if !updateApprovalGranted(cfg, &state) {
			u.logDiagnostic("approval", "not_granted", "version", state.Update.LatestVersion)
			return nil
		}
		approvedVersion := state.Update.LatestVersion
		checkAt := u.now().UTC()
		inside, checkDay, _, err := updateWindow(checkAt, cfg.Update)
		if err != nil {
			return err
		}
		// Sub2API's update endpoint installs its latest release, without a
		// target-version parameter. Recheck immediately before the attempt so a
		// newly published version requires its own Telegram decision.
		if err := u.checkVersion(ctx, cfg, client, checkAt, inside, checkDay); err != nil {
			return err
		}
		state, err = u.store.Snapshot()
		if err != nil {
			return err
		}
		if !state.Update.HasUpdate || state.Update.LatestVersion != approvedVersion || !updateApprovalGranted(cfg, &state) {
			u.logDiagnostic("approval", "version_changed", "approved_version", approvedVersion,
				"latest_version", state.Update.LatestVersion)
			return nil
		}
		latest, exists, err := client.LatestUsage(ctx)
		if err != nil {
			u.logDiagnostic("idle_check", "read_failed", "step", "after_version_preflight")
			return u.recordUpdateError(cfg, "waiting_idle", fmt.Errorf("版本复查后读取使用记录失败: %w", err))
		}
		idle, err := u.observeIdle(cfg, u.now().UTC(), latest, exists)
		if err != nil || !idle {
			return err
		}
		state, err = u.store.Snapshot()
		if err != nil {
			return err
		}
		if !state.Update.HasUpdate || state.Update.LatestVersion != approvedVersion || !updateApprovalGranted(cfg, &state) {
			return nil
		}
	}
	now = u.now().UTC()
	_, day, _, err = updateWindow(now, cfg.Update)
	if err != nil {
		return err
	}
	opID, err := randomUpdateID()
	if err != nil {
		return err
	}
	previousUpdate := state.Update
	if err := u.store.UpdateForConfigWithCurrent(cfg, func(currentState *State, currentConfig Config) error {
		if !sameUpdateTriggerSettings(currentConfig.Update, cfg.Update) ||
			cfg.Update.NotifyAvailableTelegramEnabled && !updateApprovalGranted(currentConfig, currentState) ||
			currentState.Update.LatestVersion != state.Update.LatestVersion || !currentState.Update.HasUpdate ||
			updateVersionDeclined(currentState) {
			return errUpdateApprovalChanged
		}
		s := &currentState.Update
		s.Status = "updating"
		s.LastAttemptAt = now
		s.LastAttemptDate = day
		s.LastAttemptVersion = s.LatestVersion
		s.LastTrigger = trigger
		s.LastError = ""
		s.OperationID = opID
		return nil
	}); err != nil {
		if errors.Is(err, errUpdateApprovalChanged) {
			u.logDiagnostic("approval", "changed_before_update")
			return nil
		}
		return err
	}
	restoreAttempt := func() error {
		return u.store.Update(func(state *State) error {
			if state.Update.Status == "updating" && state.Update.OperationID == opID {
				state.Update = previousUpdate
			}
			return nil
		})
	}
	current, err = u.store.Config()
	if err != nil {
		return errors.Join(err, restoreAttempt())
	}
	if current.BaseURL != cfg.BaseURL || current.AdminAPIKey != cfg.AdminAPIKey || !sameUpdateTriggerSettings(current.Update, cfg.Update) ||
		cfg.Update.NotifyAvailableTelegramEnabled && manualTelegramFingerprint(current.Telegram) != manualTelegramFingerprint(cfg.Telegram) {
		u.logDiagnostic("update", "config_changed")
		return restoreAttempt()
	}
	if cfg.Update.NotifyAvailableTelegramEnabled {
		latestState, err := u.store.Snapshot()
		if err != nil {
			return errors.Join(err, restoreAttempt())
		}
		if !updateApprovalGranted(current, &latestState) || latestState.Update.LatestVersion != state.Update.LatestVersion ||
			updateVersionDeclined(&latestState) {
			u.logDiagnostic("approval", "changed_before_request")
			return restoreAttempt()
		}
	}
	// Persisting the attempt can cross the end of the scheduled window.
	// Check once more immediately before sending the update request.
	if !cfg.Update.IdleEnabled {
		inside, _, _, err := updateWindow(u.now().UTC(), cfg.Update)
		if err != nil || !inside {
			u.logDiagnostic("update", "window_closed")
			return errors.Join(err, restoreAttempt())
		}
	}
	u.logger.Info("Sub2API 自动更新开始", "trigger", trigger, "target_version", state.Update.LatestVersion)
	u.logDiagnostic("update", "requested", "trigger", trigger, "target_version", state.Update.LatestVersion)
	result, err := client.PerformSystemUpdate(ctx, opID+"-update")
	if err != nil {
		u.logDiagnostic("update", mutationOutcome(err))
		return u.recordUpdateError(cfg, mutationOutcome(err), err)
	}
	if result.AlreadyUpToDate {
		u.logDiagnostic("update", "already_up_to_date")
		return u.store.UpdateForConfig(cfg, func(state *State) error {
			s := &state.Update
			s.Status = "up_to_date"
			s.HasUpdate = false
			s.LastError = ""
			state.UpdateApproval.Status = "superseded"
			return nil
		})
	}
	if !result.NeedRestart {
		u.logDiagnostic("update", "restart_not_confirmed")
		return u.recordUpdateError(cfg, "unknown", errors.New("更新接口未确认需要重启，请在 Sub2API 核对版本"))
	}
	u.logDiagnostic("update", "applied")
	if err := u.setUpdateState(cfg, func(s *UpdateState) { s.Status = "restarting" }); err != nil {
		return err
	}
	u.logDiagnostic("restart", "requested")
	restartErr := client.RestartSystem(ctx, opID+"-restart")
	if restartErr != nil {
		u.logDiagnostic("restart", "response_error")
		u.logger.Warn("Sub2API 重启响应异常，等待版本核对", "error", restartErr)
	} else {
		u.logDiagnostic("restart", "accepted")
	}
	verified, verifyErr := u.verifyRestart(ctx, client, state.Update.CurrentVersion, state.Update.LatestVersion)
	if verified != "" {
		u.logDiagnostic("version_verify", "confirmed", "running_version", verified)
		return u.markSuccess(cfg, verified, u.now().UTC())
	}
	u.logDiagnostic("version_verify", "not_confirmed", "target_version", state.Update.LatestVersion)
	if restartErr != nil {
		return u.recordUpdateError(cfg, mutationOutcome(restartErr), restartErr)
	}
	if verifyErr != nil && errors.Is(verifyErr, context.Canceled) {
		return verifyErr
	}
	return u.recordUpdateError(cfg, "unknown", errors.New("重启后未能确认新版本，请在 Sub2API 核对运行版本"))
}

func (u *Updater) verifyRestart(ctx context.Context, client updateAPI, oldVersion, targetVersion string) (string, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, updateVerifyTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-verifyCtx.Done():
			return "", verifyCtx.Err()
		case <-ticker.C:
			version, err := client.RunningVersion(verifyCtx)
			// PerformUpdate fetches the latest release again, so it may install a
			// version newer than the one found by our earlier availability check.
			if err == nil && version != "" && version != oldVersion && versionAtLeast(version, targetVersion) {
				return version, nil
			}
		}
	}
}

func (u *Updater) reconcile(ctx context.Context, cfg Config, client updateAPI, state UpdateState, now time.Time) error {
	version, err := client.RunningVersion(ctx)
	if err == nil && version != "" && version != state.CurrentVersion && versionAtLeast(version, state.LastAttemptVersion) {
		u.logDiagnostic("version_verify", "confirmed", "running_version", version, "step", "reconcile")
		return u.markSuccess(cfg, version, now)
	}
	if state.Status == "unknown" {
		return nil
	}
	if now.Sub(state.LastAttemptAt) < 20*time.Minute {
		return nil
	}
	u.logDiagnostic("version_verify", "not_confirmed", "step", "reconcile")
	return u.recordUpdateError(cfg, "unknown", errors.New("自动更新结果未知，请在 Sub2API 核对运行版本"))
}

// Sub2API release versions are numeric dotted versions. Exact equality also
// supports nonstandard version strings, while an unparseable different string
// cannot verify that the requested release was installed.
func versionAtLeast(actual, target string) bool {
	if actual == target && actual != "" {
		return true
	}
	parse := func(value string) ([]int, bool) {
		value = strings.TrimPrefix(value, "v")
		parts := strings.Split(value, ".")
		if len(parts) == 0 {
			return nil, false
		}
		numbers := make([]int, len(parts))
		for i, part := range parts {
			if part == "" {
				return nil, false
			}
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 {
				return nil, false
			}
			numbers[i] = number
		}
		return numbers, true
	}
	a, okA := parse(actual)
	b, okB := parse(target)
	if !okA || !okB {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var current, expected int
		if i < len(a) {
			current = a[i]
		}
		if i < len(b) {
			expected = b[i]
		}
		if current != expected {
			return current > expected
		}
	}
	return true
}

func (u *Updater) markSuccess(cfg Config, version string, now time.Time) error {
	err := u.store.UpdateForConfigWithCurrent(cfg, func(state *State, current Config) error {
		s := &state.Update
		if s.Status == "success" && s.CurrentVersion == version {
			return nil
		}
		previousVersion := s.CurrentVersion
		trigger := s.LastTrigger
		operationID := s.OperationID
		s.Status = "success"
		s.CurrentVersion = version
		s.LatestVersion = version
		s.HasUpdate = false
		state.UpdateApproval.Status = "superseded"
		s.LastSuccessAt = now
		s.LastError = ""
		if operationID == "" {
			return nil
		}
		message := updateSuccessMessage(previousVersion, version, trigger, now, current.Update.Timezone)
		if current.Update.NotifyTelegramEnabled && current.Telegram.Enabled {
			queueUpdateSuccessDelivery(state, operationID, "telegram", message, now)
		}
		if current.Update.NotifyEmailEnabled && current.Email.Enabled {
			queueUpdateSuccessDelivery(state, operationID, "email", message, now)
		}
		return nil
	})
	if err == nil {
		u.logger.Info("Sub2API 自动更新完成", "version", version)
	}
	return err
}

func queueUpdateSuccessDelivery(state *State, operationID, channel, message string, now time.Time) {
	id := "sub2api-update-" + operationID + "-" + channel
	for _, item := range state.Deliveries {
		if item.ID == id {
			return
		}
	}
	state.Deliveries = append(state.Deliveries, Delivery{
		ID: id, Kind: "update_success", Channel: channel, Message: message,
		Status: "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	})
}

func updateSuccessMessage(previousVersion, version, trigger string, at time.Time, timezone string) string {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		location = time.UTC
		timezone = "UTC"
	}
	method := "自动更新"
	switch trigger {
	case "idle":
		method = "空闲触发"
	case "scheduled":
		method = "定时触发"
	}
	message := "Sub2API 自动更新成功\n当前版本：" + version
	if previousVersion != "" && previousVersion != version {
		message += "\n原版本：" + previousVersion
	}
	return message + "\n触发方式：" + method + "\n完成时间：" + at.In(location).Format("2006-01-02 15:04:05") + "（" + timezone + "）"
}

func sameUpdateTriggerSettings(a, b UpdateConfig) bool {
	return a.IdleEnabled == b.IdleEnabled && a.ScheduledEnabled == b.ScheduledEnabled &&
		a.NotifyAvailableTelegramEnabled == b.NotifyAvailableTelegramEnabled &&
		a.WindowStart == b.WindowStart && a.WindowEnd == b.WindowEnd && a.Timezone == b.Timezone
}

func (u *Updater) recordUpdateError(cfg Config, status string, err error) error {
	if err == nil {
		return nil
	}
	u.logger.Warn("Sub2API 自动更新状态", "status", status, "error", err)
	storeErr := u.setUpdateState(cfg, func(s *UpdateState) {
		s.Status = status
		s.LastError = err.Error()
	})
	return errors.Join(err, storeErr)
}

func (u *Updater) recordCheckError(cfg Config, now time.Time, err error) error {
	u.logger.Warn("Sub2API 自动更新状态", "status", "check_failed", "error", err)
	storeErr := u.setUpdateState(cfg, func(s *UpdateState) {
		s.Status = "check_failed"
		s.LastCheckAt = now
		s.HasUpdate = false
		s.EmptySince = time.Time{}
		s.LastError = err.Error()
	})
	return errors.Join(err, storeErr)
}

func (u *Updater) setUpdateState(cfg Config, change func(*UpdateState)) error {
	return u.store.UpdateForConfig(cfg, func(state *State) error {
		change(&state.Update)
		return nil
	})
}

func (u *Updater) logDiagnostic(phase, status string, attrs ...any) {
	if !u.store.VerboseLoggingEnabled() {
		return
	}
	fields := make([]any, 0, 4+len(attrs))
	fields = append(fields, "phase", phase, "status", status)
	fields = append(fields, attrs...)
	u.logger.Info("Sub2API 自动更新", fields...)
}

func mutationOutcome(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Unknown {
		return "unknown"
	}
	return "failed"
}

func randomUpdateID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "quota-watch-" + hex.EncodeToString(raw[:]), nil
}

func updateWindow(now time.Time, cfg UpdateConfig) (bool, string, time.Time, error) {
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return false, "", time.Time{}, err
	}
	local := now.In(location)
	day := local.Format("2006-01-02")
	startClock, err := time.Parse("15:04", cfg.WindowStart)
	if err != nil {
		return false, "", time.Time{}, err
	}
	endClock, err := time.Parse("15:04", cfg.WindowEnd)
	if err != nil {
		return false, "", time.Time{}, err
	}
	start := time.Date(local.Year(), local.Month(), local.Day(), startClock.Hour(), startClock.Minute(), 0, 0, location)
	end := time.Date(local.Year(), local.Month(), local.Day(), endClock.Hour(), endClock.Minute(), 0, 0, location)
	// A wall-clock time may not exist on a daylight-saving transition day.
	// Skip that day's scheduled window instead of shifting it to another hour.
	if !sameLocalClock(start, local, startClock) || !sameLocalClock(end, local, endClock) {
		return false, day, time.Time{}, nil
	}
	return cfg.ScheduledEnabled && !local.Before(start) && local.Before(end), day, start.UTC(), nil
}

func sameLocalClock(candidate, day, clock time.Time) bool {
	return candidate.Year() == day.Year() && candidate.Month() == day.Month() && candidate.Day() == day.Day() &&
		candidate.Hour() == clock.Hour() && candidate.Minute() == clock.Minute()
}
