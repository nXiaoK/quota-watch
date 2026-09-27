package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type engineStore interface {
	Config() (Config, error)
	Snapshot() (State, error)
	Update(func(*State) error) error
}

type Engine struct {
	store             engineStore
	logger            *slog.Logger
	busy              atomic.Bool
	verboseLogging    atomic.Bool
	NewAdmin          func(Config) (AdminAPI, error)
	Notify            func(context.Context, Config, string, string) error
	NotifyInteractive func(context.Context, Config, Delivery) error
	Now               func() time.Time
	wait              func(context.Context, time.Duration) error
}

var errEngineBusy = errors.New("已有检测或重置任务正在执行")

func NewEngine(store *Store, logger *slog.Logger) *Engine {
	return newEngine(store, logger)
}

func newEngine(store engineStore, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{
		store: store, logger: logger, Now: time.Now, Notify: SendNotification, NotifyInteractive: SendInteractiveNotification,
		NewAdmin: func(cfg Config) (AdminAPI, error) {
			client, err := NewAdminClient(cfg)
			if err != nil {
				return nil, err
			}
			client.logger = logger
			if settings, ok := store.(interface{ VerboseLoggingEnabled() bool }); ok {
				client.logEnabled = settings.VerboseLoggingEnabled
			}
			return client, nil
		},
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func (e *Engine) IsBusy() bool { return e.busy.Load() }

func (e *Engine) diagnosticLogging(cfg Config) bool {
	if settings, ok := e.store.(interface{ VerboseLoggingEnabled() bool }); ok {
		return settings.VerboseLoggingEnabled()
	}
	return cfg.VerboseLogging
}

func (e *Engine) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		started := e.Now()
		if err := e.checkNow(ctx, "automatic"); err != nil && !errors.Is(err, errEngineBusy) && ctx.Err() == nil {
			e.logger.Warn("检测未全部完成", "error", err)
		}
		cfg, err := e.store.Config()
		delay := time.Minute
		if err == nil && cfg.PollIntervalSeconds > 0 {
			delay = time.Duration(cfg.PollIntervalSeconds) * time.Second
		}
		delay = started.Add(delay).Sub(e.Now())
		if delay > 0 {
			if err := e.wait(ctx, delay); err != nil {
				return
			}
		}
	}
}

// CheckNow serializes detection and reset execution. Notifications retain their
// own retry schedule and never cause an already completed reset to run again.
func (e *Engine) CheckNow(ctx context.Context) error {
	return e.checkNow(ctx, "manual")
}

func (e *Engine) checkNow(ctx context.Context, mode string) (resultErr error) {
	if !e.busy.CompareAndSwap(false, true) {
		if e.diagnosticLogging(Config{VerboseLogging: e.verboseLogging.Load()}) {
			e.logger.Info("检测周期", "phase", "end", "mode", mode, "status", "busy", "checked_accounts", 0)
		}
		return errEngineBusy
	}
	defer e.busy.Store(false)
	if err := e.recoverInFlight(); err != nil {
		return err
	}
	cfg, err := e.store.Config()
	if err != nil {
		return err
	}
	e.verboseLogging.Store(cfg.VerboseLogging)
	started := time.Now()
	cycleStatus := "checking"
	checked, skippedBackoff := 0, 0
	enabledRules, monitored := 0, 0
	defer func() {
		if !e.diagnosticLogging(cfg) {
			return
		}
		if cycleStatus == "checking" {
			cycleStatus = "completed"
			if resultErr != nil {
				cycleStatus = "error"
			}
		}
		attrs := []any{"phase", "end", "mode", mode, "status", cycleStatus, "enabled_rules", enabledRules, "monitored_accounts", monitored, "checked_accounts", checked, "skipped_backoff", skippedBackoff, "elapsed_ms", time.Since(started).Milliseconds()}
		if resultErr != nil {
			attrs = append(attrs, "error_class", engineErrorClass(resultErr))
		}
		e.logger.Info("检测周期", attrs...)
	}()
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	ids := monitoredAccounts(state.Rules)
	monitored = len(ids)
	for _, rule := range state.Rules {
		if rule.Enabled {
			enabledRules++
		}
	}
	if enabledRules == 0 {
		cycleStatus = "no_enabled_rules"
	} else if monitored == 0 {
		cycleStatus = "no_accounts"
	}
	if e.diagnosticLogging(cfg) {
		e.logger.Info("检测周期", "phase", "start", "mode", mode, "enabled_rules", enabledRules, "monitored_accounts", monitored, "interval_seconds", cfg.PollIntervalSeconds, "auto_reset_enabled", cfg.AutoResetEnabled, "status", cycleStatus)
	}
	var checkErr error
	if len(ids) > 0 {
		api, factoryErr := e.NewAdmin(cfg)
		if factoryErr != nil {
			cycleStatus = "config_invalid"
			checkErr = factoryErr
		} else {
			samples := e.collectSamples(ctx, cfg, api, state, ids)
			checked, skippedBackoff = len(samples), len(ids)-len(samples)
			if checked == 0 {
				cycleStatus = "backoff"
			}
			currentCfg, configErr := e.store.Config()
			if configErr != nil {
				return configErr
			}
			if currentCfg.BaseURL != cfg.BaseURL || currentCfg.AdminAPIKey != cfg.AdminAPIKey {
				cycleStatus = "config_changed"
				return errors.New("检测期间连接配置已改变，本轮样本已丢弃")
			}
			if err := e.recordSamples(cfg, samples); err != nil {
				return err
			}
			for _, sample := range samples {
				var unavailable *SnapshotUnavailableError
				if sample.err != nil && !errors.As(sample.err, &unavailable) {
					checkErr = errors.Join(checkErr, fmt.Errorf("账号 #%d: %w", sample.id, sample.err))
				}
			}
		}
	}
	if err := e.processActions(ctx); err != nil {
		checkErr = errors.Join(checkErr, err)
	}
	if err := e.queueEventSummaries(); err != nil {
		return errors.Join(checkErr, err)
	}
	if err := e.processDeliveries(ctx); err != nil {
		checkErr = errors.Join(checkErr, err)
	}
	now := e.Now()
	if err := e.store.Update(func(state *State) error {
		state.Health.LastCheckAt = now
		state.Health.LastError = ""
		if checkErr != nil {
			state.Health.LastError = checkErr.Error()
		} else {
			state.Health.LastSuccessAt = now
		}
		return nil
	}); err != nil {
		return errors.Join(checkErr, err)
	}
	return checkErr
}

func (e *Engine) recoverInFlight() error {
	now := e.Now()
	return e.store.Update(func(state *State) error {
		for key, observation := range state.Observations {
			if observation.Last != nil && observation.Last.Source != "stored" {
				observation.Last, observation.PreviousPercent = nil, nil
				observation.Status, observation.LastError, observation.Failures = "baseline", "", 0
				observation.NextCheckAt = time.Time{}
				state.Observations[key] = observation
			}
		}
		for i := range state.Actions {
			if state.Actions[i].Status == "pending" && !actionUsesStoredEvents(state, state.Actions[i]) {
				state.Actions[i].Status, state.Actions[i].LastError = "skipped", "已切换为数据库快照监控，取消此前采样来源的待执行动作"
				state.Actions[i].Results = failedItems(state.Actions[i].SubscriptionIDs, "skipped: "+state.Actions[i].LastError)
				state.Actions[i].UpdatedAt = now
			}
			if state.Actions[i].Status == "running" {
				state.Actions[i].Status = "unknown"
				state.Actions[i].LastError = "服务中断后无法确认重置结果，请人工核对；不会自动重放"
				state.Actions[i].UpdatedAt = now
				seen := make(map[int64]bool)
				for _, result := range state.Actions[i].Results {
					seen[result.SubscriptionID] = true
				}
				for _, id := range state.Actions[i].SubscriptionIDs {
					if !seen[id] {
						state.Actions[i].Results = append(state.Actions[i].Results, ResetItem{SubscriptionID: id, Error: state.Actions[i].LastError})
					}
				}
			}
		}
		for i := range state.ManualRequests {
			request := &state.ManualRequests[i]
			if (request.Status == "pending" || request.Status == "processing") && !request.ExpiresAt.After(now) {
				invalidateManualRequest(state, i, "expired", "手动重置请求已超过 24 小时有效期", now)
			}
			updateManualRequestStatus(state, request.ID)
		}
		for i := range state.Deliveries {
			if state.Deliveries[i].Status == "running" {
				state.Deliveries[i].Status = "pending"
				state.Deliveries[i].NextAttemptAt = now
				state.Deliveries[i].UpdatedAt = now
			}
		}
		return nil
	})
}

type engineSample struct {
	id        int64
	previous  *QuotaSnapshot
	snapshot  *QuotaSnapshot
	err       error
	checkedAt time.Time
	confirmed bool
	waiting   bool
}

type engineDiagnostic struct {
	message string
	attrs   []any
}

func engineSampleAttrs(sample engineSample, status string) []any {
	attrs := []any{"account_id", sample.id, "status", status}
	if sample.previous != nil {
		attrs = append(attrs, "previous_percent", sample.previous.UsedPercent)
	}
	if sample.snapshot != nil {
		attrs = append(attrs, "current_percent", sample.snapshot.UsedPercent, "snapshot_sampled_at", time.Unix(sample.snapshot.FetchedAt, 0).UTC().Format(time.RFC3339))
	}
	if sample.err != nil {
		attrs = append(attrs, "error_class", engineErrorClass(sample.err))
	}
	return attrs
}

func engineErrorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var unavailable *SnapshotUnavailableError
	if errors.As(err, &unavailable) {
		return "snapshot_unavailable"
	}
	var apiError *APIError
	if errors.As(err, &apiError) {
		return "sub2api_request_failed"
	}
	return "operation_failed"
}

func monitoredAccounts(rules []Rule) []int64 {
	seen := make(map[int64]bool)
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		for _, id := range rule.AccountIDs {
			if id > 0 {
				seen[id] = true
			}
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (e *Engine) collectSamples(ctx context.Context, cfg Config, api AdminAPI, state State, ids []int64) []engineSample {
	now := e.Now()
	due := make([]int64, 0, len(ids))
	var nextCheckAt time.Time
	for _, id := range ids {
		next := state.Observations[strconv.FormatInt(id, 10)].NextCheckAt
		if !next.After(now) {
			due = append(due, id)
		} else if nextCheckAt.IsZero() || next.Before(nextCheckAt) {
			nextCheckAt = next
		}
	}
	if e.diagnosticLogging(cfg) {
		status := "ready"
		if len(due) == 0 {
			status = "backoff"
		}
		attrs := []any{"phase", "start", "status", status, "due_accounts", len(due), "skipped_backoff", len(ids) - len(due)}
		if !nextCheckAt.IsZero() {
			attrs = append(attrs, "next_check_at", nextCheckAt.UTC().Format(time.RFC3339))
		}
		e.logger.Info("配额快照采样", attrs...)
	}
	out := make([]engineSample, len(due))
	parallel(len(due), cfg.Concurrency, func(i int) {
		id := due[i]
		previous := state.Observations[strconv.FormatInt(id, 10)].Last
		sample := engineSample{id: id, previous: previous}
		rechecking := false
		defer func() {
			if !rechecking || !e.diagnosticLogging(cfg) {
				return
			}
			status := "not_confirmed"
			if sample.confirmed {
				status = "confirmed"
			} else if sample.err != nil {
				status = "error"
				var unavailable *SnapshotUnavailableError
				if errors.As(sample.err, &unavailable) {
					status = "unknown"
				}
			}
			attrs := append([]any{"phase", "end"}, engineSampleAttrs(sample, status)...)
			e.logger.Info("归零快照复核", attrs...)
		}()
		snapshot, err := e.readStoredQuota(ctx, api, id)
		if err != nil {
			sample.err, sample.checkedAt = err, e.Now()
			out[i] = sample
			return
		}
		if previous != nil && !sameQuotaSource(previous, &snapshot) {
			sample.snapshot, sample.checkedAt = &snapshot, e.Now()
			out[i] = sample
			return
		}
		if previous != nil && snapshot.FetchedAt < previous.FetchedAt {
			sample.err, sample.checkedAt = &SnapshotUnavailableError{Message: "配额样本时间早于最后有效样本"}, e.Now()
			out[i] = sample
			return
		}
		if previous != nil && snapshot.FetchedAt == previous.FetchedAt {
			if snapshot.UsedPercent != previous.UsedPercent || snapshot.ResetAt != previous.ResetAt {
				sample.err, sample.checkedAt = &SnapshotUnavailableError{Message: "同一数据库快照时间的用量或重置时间不一致，保留最后有效基线"}, e.Now()
			} else {
				sample.waiting, sample.snapshot, sample.checkedAt = true, &snapshot, e.Now()
			}
			out[i] = sample
			return
		}
		if previous != nil && previous.UsedPercent > 0 && snapshot.UsedPercent == 0 {
			rechecking = true
			if snapshot.FetchedAt <= previous.FetchedAt {
				sample.err, sample.checkedAt = &SnapshotUnavailableError{Message: "归零样本不是新的有效采样"}, e.Now()
				out[i] = sample
				return
			}
			delay := time.Duration(cfg.ConfirmDelaySeconds) * time.Second
			if delay <= 0 {
				delay = 5 * time.Second
			}
			if e.diagnosticLogging(cfg) {
				e.logger.Info("归零快照复核", "phase", "start", "account_id", id, "previous_percent", previous.UsedPercent, "current_percent", snapshot.UsedPercent, "snapshot_sampled_at", time.Unix(snapshot.FetchedAt, 0).UTC().Format(time.RFC3339), "delay_seconds", delay.Seconds())
			}
			if err := e.wait(ctx, delay); err != nil {
				sample.err, sample.checkedAt = err, e.Now()
				out[i] = sample
				return
			}
			confirmation, confirmErr := e.readStoredQuota(ctx, api, id)
			if confirmErr != nil {
				sample.err, sample.checkedAt = fmt.Errorf("归零复核失败: %w", confirmErr), e.Now()
				out[i] = sample
				return
			}
			if confirmation.FetchedAt < snapshot.FetchedAt {
				sample.err, sample.checkedAt = &SnapshotUnavailableError{Message: "归零复核返回了较旧的数据库快照"}, e.Now()
				out[i] = sample
				return
			}
			if sameQuotaSource(&snapshot, &confirmation) && confirmation.FetchedAt == snapshot.FetchedAt && (confirmation.UsedPercent != snapshot.UsedPercent || confirmation.ResetAt != snapshot.ResetAt) {
				sample.err, sample.checkedAt = &SnapshotUnavailableError{Message: "归零复核的同一数据库快照时间出现不同用量或重置时间"}, e.Now()
				out[i] = sample
				return
			}
			sample.confirmed = sameQuotaSource(previous, &confirmation) && sameQuotaSource(&snapshot, &confirmation) && confirmation.UsedPercent == 0
			snapshot = confirmation
		}
		sample.snapshot, sample.checkedAt = &snapshot, e.Now()
		out[i] = sample
	})
	return out
}

func (e *Engine) readStoredQuota(ctx context.Context, api AdminAPI, id int64) (QuotaSnapshot, error) {
	account, err := api.GetAccount(ctx, id)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	snapshot, err := api.ReadStoredQuota(account)
	if err != nil {
		var unavailable *SnapshotUnavailableError
		if errors.As(err, &unavailable) {
			return QuotaSnapshot{}, err
		}
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: err.Error()}
	}
	if snapshot.Source != "stored" || snapshot.AccountID != id || strings.TrimSpace(snapshot.Identity) == "" || strings.TrimSpace(snapshot.Plan) == "" || strings.TrimSpace(snapshot.Dimension) == "" || snapshot.FetchedAt <= 0 || math.IsNaN(snapshot.UsedPercent) || math.IsInf(snapshot.UsedPercent, 0) || snapshot.UsedPercent < 0 {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "数据库配额快照缺少有效来源、身份、套餐、7d 维度、采样时间或原始用量"}
	}
	if snapshot.FetchedAt > e.Now().Add(30*time.Second).Unix() {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "数据库配额快照时间超前，请检查服务与主站时钟"}
	}
	return snapshot, nil
}

func sameQuotaSource(a, b *QuotaSnapshot) bool {
	return a != nil && b != nil && a.AccountID == b.AccountID && a.Source == b.Source && a.Identity == b.Identity && a.Plan == b.Plan && a.Dimension == b.Dimension
}

func sameBaseline(a, b *QuotaSnapshot) bool {
	return (a == nil && b == nil) || (sameQuotaSource(a, b) && a.FetchedAt == b.FetchedAt && a.UsedPercent == b.UsedPercent && a.ResetAt == b.ResetAt)
}

func (e *Engine) recordSamples(cfg Config, samples []engineSample) error {
	update := e.store.Update
	if scoped, ok := e.store.(interface {
		UpdateForConfig(Config, func(*State) error) error
	}); ok {
		update = func(fn func(*State) error) error { return scoped.UpdateForConfig(cfg, fn) }
	}
	logs := make([]engineDiagnostic, 0)
	err := update(func(state *State) error {
		if state.Observations == nil {
			state.Observations = make(map[string]Observation)
		}
		newEvents := make([]Event, 0)
		eventRules := make(map[string][]Rule)
		for _, sample := range samples {
			key := strconv.FormatInt(sample.id, 10)
			observation := state.Observations[key]
			if !sameBaseline(observation.Last, sample.previous) {
				continue
			}
			observation.AccountID, observation.CheckedAt = sample.id, sample.checkedAt
			rules := sourceRules(state.Rules, sample.id)
			if sample.err != nil {
				var unavailable *SnapshotUnavailableError
				if errors.As(sample.err, &unavailable) {
					observation.Status, observation.LastError, observation.Failures = "unknown", sample.err.Error(), 0
					observation.NextCheckAt = time.Time{}
					state.Observations[key] = observation
					if e.diagnosticLogging(cfg) {
						logs = append(logs, engineDiagnostic{"配额快照", engineSampleAttrs(sample, observation.Status)})
					}
					continue
				}
				firstFailure := observation.Failures == 0
				observation.Status, observation.LastError = "error", sample.err.Error()
				observation.Failures++
				observation.NextCheckAt = sample.checkedAt.Add(errorBackoff(cfg, sample.err, observation.Failures))
				if firstFailure {
					message := fmt.Sprintf("账号配额监控异常\n账号：%s (#%d)\n原因：%s\n最后有效样本已保留，未触发订阅重置。", observation.AccountName, sample.id, sample.err)
					e.addDeliveries(state, "error-"+hashValue([]any{sample.id, sample.checkedAt.UnixNano()}), "", notificationChannels(cfg, rules), message, sample.checkedAt)
				}
				state.Observations[key] = observation
				if e.diagnosticLogging(cfg) {
					attrs := append(engineSampleAttrs(sample, observation.Status), "failures", observation.Failures, "next_check_at", observation.NextCheckAt.UTC().Format(time.RFC3339))
					logs = append(logs, engineDiagnostic{"配额快照", attrs})
				}
				continue
			}
			if sample.snapshot == nil {
				continue
			}
			if observation.Failures > 0 {
				message := fmt.Sprintf("账号配额监控已恢复\n账号：%s (#%d)\n当前原始 7d 用量：%g%%", sample.snapshot.AccountName, sample.id, sample.snapshot.UsedPercent)
				e.addDeliveries(state, "recovery-"+hashValue([]any{sample.id, sample.checkedAt.UnixNano()}), "", notificationChannels(cfg, rules), message, sample.checkedAt)
			}
			if observation.Last != nil {
				value := observation.Last.UsedPercent
				observation.PreviousPercent = &value
			}
			observation.Last, observation.AccountName = sample.snapshot, sample.snapshot.AccountName
			observation.LastError, observation.Failures = "", 0
			observation.NextCheckAt = time.Time{}
			observation.Status = "watching"
			if sample.waiting {
				observation.Status = "waiting"
			}
			if sample.previous == nil || !sameQuotaSource(sample.previous, sample.snapshot) {
				observation.Status = "baseline"
			}
			if sample.confirmed && len(rules) > 0 {
				observation.Status = "reset"
				event := Event{
					ID:        "event-" + hashValue([]any{cfg.BaseURL, sample.id, sample.previous, sample.snapshot}),
					AccountID: sample.id, AccountName: sample.snapshot.AccountName,
					Source: sample.snapshot.Source, SampledAt: sample.snapshot.FetchedAt,
					Dimension: sample.snapshot.Dimension, PreviousPercent: sample.previous.UsedPercent,
					UsedPercent: sample.snapshot.UsedPercent, ResetAt: sample.snapshot.ResetAt, DetectedAt: sample.checkedAt,
					RuleNames: make([]string, 0, len(rules)),
				}
				event.NotificationChannels = notificationChannels(cfg, rules)
				for _, rule := range rules {
					event.RuleNames = append(event.RuleNames, rule.Name)
				}
				exists := false
				for _, prior := range state.Events {
					if prior.ID == event.ID {
						exists = true
						break
					}
				}
				if !exists {
					state.Events = append(state.Events, event)
					newEvents = append(newEvents, event)
					eventRules[event.ID] = rules
					if e.diagnosticLogging(cfg) {
						logs = append(logs, engineDiagnostic{"归零事件已保存", []any{"account_id", event.AccountID, "previous_percent", event.PreviousPercent, "current_percent", event.UsedPercent, "snapshot_sampled_at", time.Unix(event.SampledAt, 0).UTC().Format(time.RFC3339), "matched_rules", len(rules)}})
					}
				}
			}
			state.Observations[key] = observation
			if e.diagnosticLogging(cfg) {
				logs = append(logs, engineDiagnostic{"配额快照", engineSampleAttrs(sample, observation.Status)})
			}
		}
		actions := buildBatchEventActions(cfg, newEvents, eventRules, e.Now())
		state.Actions = append(state.Actions, actions...)
		supersedeManualRequests(state, newEvents, eventRules, e.Now())
		request, err := buildManualRequest(cfg, newEvents, eventRules, e.Now())
		if err != nil {
			return err
		}
		if request != nil {
			state.ManualRequests = append(state.ManualRequests, *request)
			state.Deliveries = append(state.Deliveries, Delivery{ID: "manual-" + request.ID + "-telegram", ManualRequestID: request.ID, EventID: request.EventIDs[0], Channel: "telegram", Message: manualRequestMessage(state, *request), Status: "pending", NextAttemptAt: request.CreatedAt, CreatedAt: request.CreatedAt, UpdatedAt: request.CreatedAt})
			if e.diagnosticLogging(cfg) {
				logs = append(logs, engineDiagnostic{"手动重置确认待处理", []any{"status", "pending", "events", len(request.EventIDs), "subscriptions", len(request.Targets)}})
			}
		}
		if len(newEvents) > 0 && e.diagnosticLogging(cfg) {
			logs = append(logs, engineDiagnostic{"归零处理已排队", []any{"events", len(newEvents), "automatic_actions", len(actions), "manual_confirmation_pending", request != nil}})
		}
		for _, event := range newEvents {
			hasAction := false
			for _, action := range actions {
				if actionMatchesEvent(action, event.ID) {
					hasAction = true
					break
				}
			}
			if !hasAction {
				channels := append([]string(nil), event.NotificationChannels...)
				if request != nil && containsString(request.EventIDs, event.ID) {
					channels = make([]string, 0, len(event.NotificationChannels))
					for _, channel := range event.NotificationChannels {
						if channel != "telegram" {
							channels = append(channels, channel)
						}
					}
				}
				e.addDeliveries(state, event.ID+"-summary", event.ID, channels, eventMessage(event)+"\n订阅自动重置未执行：全局/规则开关关闭，或没有选择订阅。", event.DetectedAt)
			}
		}
		return nil
	})
	if err == nil && e.diagnosticLogging(cfg) {
		for _, entry := range logs {
			e.logger.Info(entry.message, entry.attrs...)
		}
	}
	return err
}

func sourceRules(rules []Rule, id int64) []Rule {
	out := make([]Rule, 0)
	for _, rule := range rules {
		if rule.Enabled && containsID(rule.AccountIDs, id) {
			out = append(out, rule)
		}
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func notificationChannels(cfg Config, rules []Rule) []string {
	telegram, email := false, false
	for _, rule := range rules {
		telegram = telegram || rule.NotifyTelegram
		email = email || rule.NotifyEmail
	}
	out := make([]string, 0, 2)
	if telegram && cfg.Telegram.Enabled {
		out = append(out, "telegram")
	}
	if email && cfg.Email.Enabled {
		out = append(out, "email")
	}
	return out
}

func buildEventActions(cfg Config, event Event, rules []Rule, now time.Time) []Action {
	if event.NotificationChannels == nil {
		event.NotificationChannels = notificationChannels(cfg, rules)
	}
	return buildBatchEventActions(cfg, []Event{event}, map[string][]Rule{event.ID: rules}, now)
}

// Coalesce only the events confirmed by one sampling pass. Each subscription
// receives the union of its selected windows once, including across sources.
func buildBatchEventActions(cfg Config, events []Event, eventRules map[string][]Rule, now time.Time) []Action {
	if !cfg.AutoResetEnabled {
		return nil
	}
	type target struct {
		mask                    ResetMask
		rules, events, channels []string
	}
	targets := make(map[int64]*target)
	ruleMap := make(map[string]Rule)
	for _, event := range events {
		for _, rule := range eventRules[event.ID] {
			ruleMap[rule.ID] = rule
			if !rule.AutoReset || (!rule.Daily && !rule.Weekly && !rule.Monthly) {
				continue
			}
			for _, id := range rule.SubscriptionIDs {
				if id <= 0 {
					continue
				}
				if targets[id] == nil {
					targets[id] = &target{}
				}
				item := targets[id]
				item.mask.Daily = item.mask.Daily || rule.Daily
				item.mask.Weekly = item.mask.Weekly || rule.Weekly
				item.mask.Monthly = item.mask.Monthly || rule.Monthly
				if !containsString(item.rules, rule.ID) {
					item.rules = append(item.rules, rule.ID)
				}
				if !containsString(item.events, event.ID) {
					item.events = append(item.events, event.ID)
				}
				for _, channel := range event.NotificationChannels {
					if !containsString(item.channels, channel) {
						item.channels = append(item.channels, channel)
					}
				}
			}
		}
	}
	type group struct {
		mask                    ResetMask
		rules, events, channels []string
		ids                     []int64
	}
	groups := make(map[string]*group)
	for id, item := range targets {
		sort.Strings(item.rules)
		sort.Strings(item.events)
		sort.Strings(item.channels)
		key := hashValue([]any{item.mask, item.rules, item.events})
		if groups[key] == nil {
			groups[key] = &group{mask: item.mask, rules: item.rules, events: item.events, channels: item.channels}
		}
		groups[key].ids = append(groups[key].ids, id)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Action, 0)
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group.ids, func(i, j int) bool { return group.ids[i] < group.ids[j] })
		for start := 0; start < len(group.ids); start += 100 {
			end := min(start+100, len(group.ids))
			ids := append([]int64(nil), group.ids[start:end]...)
			id := "action-" + hashValue([]any{group.events, ids, group.mask, group.rules})
			out = append(out, Action{
				ID: id, EventID: group.events[0], EventIDs: append([]string(nil), group.events...), RuleIDs: append([]string(nil), group.rules...), SubscriptionIDs: ids,
				Mask: group.mask, IdempotencyKey: actionKey(cfg, ruleMap, group.rules, id),
				NotificationChannels: append([]string(nil), group.channels...), Status: "pending", Results: []ResetItem{}, CreatedAt: now, UpdatedAt: now,
			})
		}
	}
	return out
}

func actionMatchesEvent(action Action, eventID string) bool {
	if len(action.EventIDs) > 0 {
		return containsString(action.EventIDs, eventID)
	}
	return action.EventID == eventID
}

func actionRuleHash(cfg Config, rules map[string]Rule, ids []string) string {
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	payload := []any{cfg.BaseURL}
	for _, id := range ordered {
		rule, found := rules[id]
		if !found {
			payload = append(payload, id, "missing")
			continue
		}
		accounts := uniqueSortedIDs(rule.AccountIDs)
		subscriptions := uniqueSortedIDs(rule.SubscriptionIDs)
		payload = append(payload, id, rule.Enabled, rule.AutoReset, accounts, subscriptions, ResetMask{Daily: rule.Daily, Weekly: rule.Weekly, Monthly: rule.Monthly})
	}
	return hashValue(payload)[:24]
}

func actionKey(cfg Config, rules map[string]Rule, ids []string, actionID string) string {
	return "quota-watch." + actionRuleHash(cfg, rules, ids) + "." + hashValue(actionID)
}

func actionAllowed(cfg Config, state *State, action Action) bool {
	if action.Mode == "manual" || action.ManualRequestID != "" {
		return manualActionAllowed(cfg, state, action)
	}
	if !cfg.AutoResetEnabled || len(action.RuleIDs) == 0 || !actionUsesStoredEvents(state, action) {
		return false
	}
	rules := make(map[string]Rule)
	for _, rule := range state.Rules {
		rules[rule.ID] = rule
	}
	for _, id := range action.RuleIDs {
		rule, found := rules[id]
		if !found || !rule.Enabled || !rule.AutoReset {
			return false
		}
	}
	prefix := "quota-watch." + actionRuleHash(cfg, rules, action.RuleIDs) + "."
	return strings.HasPrefix(action.IdempotencyKey, prefix)
}

func (e *Engine) processActions(ctx context.Context) error {
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	var allErr error
	for _, action := range state.Actions {
		if action.Status != "pending" {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(allErr, ctx.Err())
		}
		if err := e.executeAction(ctx, action.ID); err != nil {
			allErr = errors.Join(allErr, err)
		}
	}
	return allErr
}

func (e *Engine) executeAction(ctx context.Context, id string) error {
	cfg, err := e.store.Config()
	if err != nil {
		return err
	}
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	index := actionIndex(&state, id)
	if index < 0 || state.Actions[index].Status != "pending" {
		return nil
	}
	action := state.Actions[index]
	mode := "automatic"
	if action.Mode == "manual" || action.ManualRequestID != "" {
		mode = "manual"
	}
	if e.diagnosticLogging(cfg) {
		e.logger.Info("订阅重置动作", "phase", "start", "mode", mode, "status", "validating", "subscriptions", len(action.SubscriptionIDs))
	}
	if !actionAllowedAt(cfg, &state, action, e.Now()) {
		return e.finishAction(cfg, id, "skipped", failedItems(action.SubscriptionIDs, "skipped: 重置授权、有效期或规则目标/周期已改变"), "重置授权、有效期或规则目标/周期已改变")
	}
	api, err := e.NewAdmin(cfg)
	if err != nil {
		return e.failBeforeWrite(cfg, action, err)
	}
	hints := primeEngineSubscriptionHints(api, state.Rules, action.SubscriptionIDs)
	if action.Mode == "manual" {
		hints = manualSubscriptionHints(api, state.ManualRequests[manualRequestIndex(&state, action.ManualRequestID)], action.SubscriptionIDs)
	}
	subscriptions, err := api.GetSubscriptions(ctx, action.SubscriptionIDs)
	if err != nil {
		return e.failBeforeWrite(cfg, action, fmt.Errorf("执行前订阅核对失败: %w", err))
	}
	eligible, skipped := eligibleSubscriptions(action.SubscriptionIDs, subscriptions, e.Now(), hints)
	if len(eligible) == 0 {
		return e.finishAction(cfg, id, "skipped", skipped, "没有当前有效的目标订阅")
	}
	currentCfg, err := e.store.Config()
	if err != nil {
		return err
	}
	claimed := false
	if err := e.store.Update(func(state *State) error {
		index := actionIndex(state, id)
		if index < 0 || state.Actions[index].Status != "pending" {
			return nil
		}
		item := &state.Actions[index]
		if !actionAllowedAt(currentCfg, state, *item, e.Now()) {
			item.Status, item.LastError = "skipped", "执行前重置授权、有效期或规则目标/周期已改变"
			item.Results = failedItems(item.SubscriptionIDs, "skipped: "+item.LastError)
			item.UpdatedAt = e.Now()
			updateManualRequestStatus(state, item.ManualRequestID)
			return nil
		}
		item.Status, item.Results, item.UpdatedAt = "running", skipped, e.Now()
		item.Attempts++
		claimed = true
		return nil
	}); err != nil {
		return err
	}
	if !claimed {
		if e.diagnosticLogging(cfg) {
			e.logger.Info("订阅重置动作", "phase", "end", "mode", mode, "status", "not_claimed", "subscriptions", len(action.SubscriptionIDs))
		}
		return nil
	}
	currentCfg, err = e.store.Config()
	if err != nil {
		return err
	}
	currentState, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	if !actionAllowedAt(currentCfg, &currentState, action, e.Now()) {
		return e.finishAction(currentCfg, id, "skipped", failedItems(action.SubscriptionIDs, "skipped: 重置请求发出前授权、有效期或规则已改变"), "重置请求发出前授权、有效期或规则已改变")
	}
	result, resetErr := api.ResetSubscriptions(ctx, eligible, action.Mask, action.IdempotencyKey)
	if resetErr == nil {
		resetErr = validateEngineResetResult(eligible, result)
	}
	if resetErr != nil {
		status := "unknown"
		var apiError *APIError
		if errors.As(resetErr, &apiError) && !apiError.Unknown && apiError.Status >= 400 && apiError.Status < 500 && apiError.Status != 408 && apiError.Status != 409 {
			status = "failed"
		}
		results := append(skipped, failedItems(eligible, resetErr.Error())...)
		if err := e.finishAction(currentCfg, id, status, orderedItems(action.SubscriptionIDs, results), resetErr.Error()); err != nil {
			return errors.Join(resetErr, err)
		}
		return resetErr
	}
	results := orderedItems(action.SubscriptionIDs, append(skipped, result.Results...))
	status := "succeeded"
	success := 0
	for _, item := range results {
		if item.Success {
			success++
		}
	}
	if success != len(results) {
		status = "partial"
	}
	if success == 0 {
		status = "failed"
	}
	return e.finishAction(currentCfg, id, status, results, "")
}

func eligibleSubscriptions(ids []int64, subscriptions map[int64]Subscription, now time.Time, hints map[string]SubscriptionRef) ([]int64, []ResetItem) {
	eligible := make([]int64, 0, len(ids))
	skipped := make([]ResetItem, 0)
	for _, id := range ids {
		subscription, found := subscriptions[id]
		reason := ""
		if !found {
			reason = "订阅不存在或已撤销"
		} else if subscription.ID != id {
			reason = "订阅 ID 不匹配"
		} else if ref, found := hints[strconv.FormatInt(id, 10)]; found && (ref.UserID != subscription.UserID || ref.GroupID != subscription.GroupID) {
			reason = "订阅用户或分组归属与选择时不一致"
		} else if subscription.Status != "active" {
			reason = "订阅状态不是 active"
		} else if !subscription.ExpiresAt.After(now) {
			reason = "订阅已过期"
		} else if subscription.StartsAt.After(now) {
			reason = "订阅尚未生效"
		}
		if reason != "" {
			skipped = append(skipped, ResetItem{SubscriptionID: id, Error: "skipped: " + reason})
		} else {
			eligible = append(eligible, id)
		}
	}
	return eligible, skipped
}

func (e *Engine) failBeforeWrite(cfg Config, action Action, cause error) error {
	if err := e.finishAction(cfg, action.ID, "failed", failedItems(action.SubscriptionIDs, cause.Error()), cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (e *Engine) finishAction(cfg Config, id, status string, results []ResetItem, message string) error {
	mode := "automatic"
	err := e.store.Update(func(state *State) error {
		index := actionIndex(state, id)
		if index < 0 {
			return errors.New("重置动作不存在")
		}
		if state.Actions[index].Mode == "manual" || state.Actions[index].ManualRequestID != "" {
			mode = "manual"
		}
		state.Actions[index].Status, state.Actions[index].Results = status, results
		state.Actions[index].LastError, state.Actions[index].UpdatedAt = message, e.Now()
		updateManualRequestStatus(state, state.Actions[index].ManualRequestID)
		return nil
	})
	if err == nil && e.diagnosticLogging(cfg) {
		succeeded, failed, skipped, unknown := 0, 0, 0, 0
		for _, result := range results {
			if result.Success {
				succeeded++
			} else if strings.HasPrefix(result.Error, "skipped:") {
				skipped++
			} else if status == "unknown" {
				unknown++
			} else {
				failed++
			}
		}
		e.logger.Info("订阅重置动作", "phase", "end", "mode", mode, "status", status, "subscriptions", len(results), "success_count", succeeded, "failed_count", failed, "skipped_count", skipped, "unknown_count", unknown)
	}
	return err
}

func actionIndex(state *State, id string) int {
	for i := range state.Actions {
		if state.Actions[i].ID == id {
			return i
		}
	}
	return -1
}

func validateEngineResetResult(ids []int64, result ResetResult) error {
	seen := make(map[int64]bool, len(ids))
	success, failed := 0, 0
	for _, item := range result.Results {
		if !containsID(ids, item.SubscriptionID) || seen[item.SubscriptionID] {
			return errors.New("重置返回的目标结果不完整或不匹配，结果未知")
		}
		seen[item.SubscriptionID] = true
		if item.Success {
			success++
		} else {
			failed++
		}
	}
	if len(seen) != len(ids) || success != result.SuccessCount || failed != result.FailedCount {
		return errors.New("重置返回的计数或逐项结果不完整，结果未知")
	}
	return nil
}

func (e *Engine) RetryAction(ctx context.Context, id string) error {
	if !e.busy.CompareAndSwap(false, true) {
		return errEngineBusy
	}
	defer e.busy.Store(false)
	if err := e.recoverInFlight(); err != nil {
		return err
	}
	cfg, err := e.store.Config()
	if err != nil {
		return err
	}
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	index := actionIndex(&state, id)
	if index < 0 {
		return errors.New("重置动作不存在")
	}
	original := state.Actions[index]
	if original.Status != "failed" && original.Status != "partial" {
		return errors.New("只能重试明确失败的目标；结果未知的动作需人工核对")
	}
	if !actionAllowedAt(cfg, &state, original, e.Now()) {
		return errors.New("重置授权、有效期、原规则或配额快照来源已改变，不能重试旧动作")
	}
	ids := make([]int64, 0)
	for _, item := range original.Results {
		if !item.Success && !strings.HasPrefix(item.Error, "skipped:") && containsID(original.SubscriptionIDs, item.SubscriptionID) {
			ids = append(ids, item.SubscriptionID)
		}
	}
	ids = uniqueSortedIDs(ids)
	if len(ids) == 0 {
		return errors.New("没有可以重试的明确失败目标")
	}
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	now := e.Now()
	retry := original
	retry.ID = "retry-" + hex.EncodeToString(entropy)
	retry.SubscriptionIDs, retry.Status, retry.Results = ids, "pending", []ResetItem{}
	retry.LastError, retry.Attempts, retry.CreatedAt, retry.UpdatedAt = "", 0, now, now
	rules := make(map[string]Rule)
	for _, rule := range state.Rules {
		rules[rule.ID] = rule
	}
	retry.IdempotencyKey = actionKey(cfg, rules, retry.RuleIDs, retry.ID)
	if retry.Mode == "manual" {
		retry.IdempotencyKey = manualActionKey(state.ManualRequests[manualRequestIndex(&state, retry.ManualRequestID)], retry.ID)
	}
	if err := e.store.Update(func(state *State) error {
		index := actionIndex(state, id)
		if index < 0 || hashValue(state.Actions[index]) != hashValue(original) {
			return errors.New("原动作已改变，请刷新后重试")
		}
		if !actionAllowedAt(cfg, state, original, e.Now()) {
			return errors.New("原规则已改变，请刷新后重试")
		}
		state.Actions[index].Status, state.Actions[index].UpdatedAt = "retried", now
		state.Actions = append(state.Actions, retry)
		if retry.Mode == "manual" {
			request := &state.ManualRequests[manualRequestIndex(state, retry.ManualRequestID)]
			request.ActionIDs = append(request.ActionIDs, retry.ID)
			request.Status = "processing"
		}
		return nil
	}); err != nil {
		return err
	}
	executionErr := e.executeAction(ctx, retry.ID)
	if err := e.queueEventSummaries(); err != nil {
		return errors.Join(executionErr, err)
	}
	return errors.Join(executionErr, e.processDeliveries(ctx))
}

func (e *Engine) queueEventSummaries() error {
	now := e.Now()
	return e.store.Update(func(state *State) error {
		for _, event := range state.Events {
			actions := make([]Action, 0)
			channels := append([]string(nil), event.NotificationChannels...)
			complete := true
			ids := make([]string, 0)
			for _, action := range state.Actions {
				if !actionMatchesEvent(action, event.ID) {
					continue
				}
				actions = append(actions, action)
				ids = append(ids, action.ID)
				if len(action.EventIDs) == 0 {
					for _, channel := range action.NotificationChannels {
						if !containsString(channels, channel) {
							channels = append(channels, channel)
						}
					}
				}
				if action.Status == "pending" || action.Status == "running" {
					complete = false
				}
			}
			if len(actions) == 0 || !complete {
				continue
			}
			sort.Strings(ids)
			e.addDeliveries(state, event.ID+"-summary-"+hashValue(ids), event.ID, channels, eventActionMessage(event, actions), now)
		}
		return nil
	})
}

func eventMessage(event Event) string {
	title := "检测到账号数据库快照的 7d 原始用量归零"
	if event.Source != "stored" {
		title = "历史账号 7d 归零事件（旧采样来源）"
	}
	message := fmt.Sprintf("%s\n账号：%s (#%d)\n用量：%g%% → %g%%\n维度：%s\n时间：%s\n规则：%s", title, event.AccountName, event.AccountID, event.PreviousPercent, event.UsedPercent, event.Dimension, event.DetectedAt.Format(time.RFC3339), strings.Join(event.RuleNames, "、"))
	if event.Source == "stored" && event.SampledAt > 0 {
		message += "\n主站快照更新时间：" + time.Unix(event.SampledAt, 0).UTC().Format(time.RFC3339) + "\n被动读取已保存快照，实际重置可能早于检测时间。"
	}
	return message
}

func eventActionMessage(event Event, actions []Action) string {
	latest := make(map[int64]ResetItem)
	unknown := make(map[int64]bool)
	for _, action := range actions {
		for _, item := range action.Results {
			latest[item.SubscriptionID] = item
			unknown[item.SubscriptionID] = action.Status == "unknown" && !strings.HasPrefix(item.Error, "skipped:")
		}
	}
	success, failed, skipped, uncertain := 0, 0, 0, 0
	for id, item := range latest {
		if unknown[id] {
			uncertain++
		} else if item.Success {
			success++
		} else if strings.HasPrefix(item.Error, "skipped:") {
			skipped++
		} else {
			failed++
		}
	}
	mode := ""
	for _, action := range actions {
		if action.Mode == "manual" {
			mode = "\n执行方式：Telegram 确认后手动重置。"
			break
		}
	}
	return eventMessage(event) + mode + fmt.Sprintf("\n订阅重置结果：成功 %d，失败 %d，跳过 %d，结果未知 %d。\n重置仅清所选周期用量，不延长订阅有效期。", success, failed, skipped, uncertain)
}

func (e *Engine) addDeliveries(state *State, prefix, eventID string, channels []string, message string, now time.Time) {
	for _, channel := range channels {
		id := prefix + "-" + channel
		exists := false
		for _, delivery := range state.Deliveries {
			if delivery.ID == id {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		state.Deliveries = append(state.Deliveries, Delivery{ID: id, EventID: eventID, Channel: channel, Message: message, Status: "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now})
	}
}

func (e *Engine) processDeliveries(ctx context.Context) error {
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	cfg, err := e.store.Config()
	if err != nil {
		return err
	}
	now := e.Now()
	due := make([]Delivery, 0)
	for _, delivery := range state.Deliveries {
		if (delivery.Status == "pending" || delivery.Status == "failed") && !delivery.NextAttemptAt.After(now) {
			due = append(due, delivery)
		}
	}
	var mutex sync.Mutex
	var allErr error
	parallel(len(due), cfg.Concurrency, func(i int) {
		if ctx.Err() != nil {
			return
		}
		if err := e.sendDelivery(ctx, due[i]); err != nil {
			mutex.Lock()
			allErr = errors.Join(allErr, err)
			mutex.Unlock()
		}
	})
	return allErr
}

func (e *Engine) sendDelivery(ctx context.Context, original Delivery) error {
	cfg, err := e.store.Config()
	if err != nil {
		return err
	}
	enabled := (original.Channel == "telegram" && cfg.Telegram.Enabled) || (original.Channel == "email" && cfg.Email.Enabled)
	claimed := false
	attempts := 0
	update := e.store.Update
	if original.ManualRequestID != "" {
		update = func(fn func(*State) error) error { return e.manualConfigUpdate(cfg, fn) }
	}
	if err := update(func(state *State) error {
		for i := range state.Deliveries {
			item := &state.Deliveries[i]
			if item.ID != original.ID || (item.Status != "pending" && item.Status != "failed") || item.NextAttemptAt.After(e.Now()) {
				continue
			}
			if !enabled {
				item.Status, item.LastError, item.UpdatedAt = "skipped", "通知渠道已关闭", e.Now()
				return nil
			}
			if item.ManualRequestID != "" {
				index := manualRequestIndex(state, item.ManualRequestID)
				if index < 0 || state.ManualRequests[index].Status != "pending" {
					item.Status, item.LastError, item.UpdatedAt = "skipped", "手动重置请求已处理或已失效", e.Now()
					return nil
				}
				request := state.ManualRequests[index]
				if !request.ExpiresAt.After(e.Now()) {
					invalidateManualRequest(state, index, "expired", "手动重置请求已超过 24 小时有效期", e.Now())
					item.Status, item.LastError, item.UpdatedAt = "skipped", "手动重置请求已过期", e.Now()
					return nil
				}
				if item.Channel != "telegram" || !manualRequestScopeValid(cfg, state, request) {
					invalidateManualRequest(state, index, "invalid", "连接、规则目标/周期或 Telegram 权限配置已改变", e.Now())
					item.Status, item.LastError, item.UpdatedAt = "skipped", "手动重置请求配置已改变", e.Now()
					return nil
				}
			}
			item.Status, item.UpdatedAt = "running", e.Now()
			item.Attempts++
			attempts, claimed = item.Attempts, true
			return nil
		}
		return nil
	}); err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	var sendErr error
	if original.ManualRequestID != "" && original.Channel == "telegram" {
		sendErr = e.NotifyInteractive(ctx, cfg, original)
	} else {
		sendErr = e.Notify(ctx, cfg, original.Channel, original.Message)
	}
	if err := e.store.Update(func(state *State) error {
		for i := range state.Deliveries {
			item := &state.Deliveries[i]
			if item.ID != original.ID {
				continue
			}
			item.UpdatedAt, item.LastError = e.Now(), ""
			if sendErr == nil {
				item.Status = "succeeded"
			} else {
				item.Status, item.LastError = "failed", sendErr.Error()
				item.NextAttemptAt = e.Now().Add(notificationBackoff(sendErr, attempts))
			}
			return nil
		}
		return errors.New("通知任务不存在")
	}); err != nil {
		return errors.Join(sendErr, err)
	}
	return sendErr
}

func (e *Engine) Simulate(ctx context.Context, rule Rule) (Simulation, error) {
	cfg, err := e.store.Config()
	if err != nil {
		return Simulation{}, err
	}
	api, err := e.NewAdmin(cfg)
	if err != nil {
		return Simulation{}, err
	}
	simulation := Simulation{Rule: rule, Accounts: []Account{}, Subscriptions: []Subscription{}, Skipped: []ResetItem{}, Message: "模拟只核对账号与订阅，不发送通知，不重置额度，也不修改监控基线。"}
	ids := uniqueSortedIDs(rule.AccountIDs)
	accounts := make([]Account, len(ids))
	accountErrors := make([]error, len(ids))
	parallel(len(ids), cfg.Concurrency, func(i int) { accounts[i], accountErrors[i] = api.GetAccount(ctx, ids[i]) })
	var allErr error
	for i, account := range accounts {
		if accountErrors[i] != nil {
			allErr = errors.Join(allErr, fmt.Errorf("账号 #%d: %w", ids[i], accountErrors[i]))
			continue
		}
		simulation.Accounts = append(simulation.Accounts, account)
		if account.Platform != "openai" || account.Type != "oauth" || (account.QuotaDimension != "" && account.QuotaDimension != "global" && account.QuotaDimension != "spark") || (account.ParentAccountID != nil && account.QuotaDimension == "global") {
			allErr = errors.Join(allErr, fmt.Errorf("账号 #%d 不是支持的 OpenAI OAuth 配额账号", account.ID))
		}
	}
	targets := uniqueSortedIDs(rule.SubscriptionIDs)
	if len(targets) > 0 {
		hints := primeEngineSubscriptionHints(api, []Rule{rule}, targets)
		subscriptions, fetchErr := api.GetSubscriptions(ctx, targets)
		if fetchErr != nil {
			return simulation, errors.Join(allErr, fetchErr)
		}
		eligible, skipped := eligibleSubscriptions(targets, subscriptions, e.Now(), hints)
		simulation.Skipped = skipped
		for _, id := range eligible {
			simulation.Subscriptions = append(simulation.Subscriptions, subscriptions[id])
		}
	}
	simulation.WillAutoReset = allErr == nil && rule.Enabled && cfg.AutoResetEnabled && rule.AutoReset && len(simulation.Accounts) > 0 && len(simulation.Subscriptions) > 0 && (rule.Daily || rule.Weekly || rule.Monthly)
	if !simulation.WillAutoReset {
		simulation.Message += " 当前开关、账号或订阅条件不满足自动重置。"
	}
	return simulation, allErr
}

func errorBackoff(cfg Config, err error, attempts int) time.Duration {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.Status == 429 && apiError.RetryAfter > 0 {
		return apiError.RetryAfter
	}
	base := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if base <= 0 {
		base = time.Minute
	}
	return boundedBackoff(base, attempts, 15*time.Minute)
}

func notificationBackoff(err error, attempts int) time.Duration {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.RetryAfter > 0 {
		return apiError.RetryAfter
	}
	return boundedBackoff(30*time.Second, attempts, time.Hour)
}

func boundedBackoff(base time.Duration, attempts int, limit time.Duration) time.Duration {
	for i := 1; i < attempts && base < limit; i++ {
		if base > limit/2 {
			return limit
		}
		base *= 2
	}
	return min(base, limit)
}

func parallel(count, concurrency int, fn func(int)) {
	if count == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = 3
	}
	concurrency = min(count, concurrency)
	var workers sync.WaitGroup
	var next atomic.Int64
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= count {
					return
				}
				fn(index)
			}
		}()
	}
	workers.Wait()
}

func uniqueSortedIDs(ids []int64) []int64 {
	seen := make(map[int64]bool)
	for _, id := range ids {
		if id > 0 {
			seen[id] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func failedItems(ids []int64, message string) []ResetItem {
	out := make([]ResetItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, ResetItem{SubscriptionID: id, Error: message})
	}
	return out
}

func orderedItems(ids []int64, items []ResetItem) []ResetItem {
	byID := make(map[int64]ResetItem, len(items))
	for _, item := range items {
		byID[item.SubscriptionID] = item
	}
	out := make([]ResetItem, 0, len(ids))
	for _, id := range ids {
		if item, found := byID[id]; found {
			out = append(out, item)
		}
	}
	return out
}

func hashValue(value any) string {
	data, _ := json.Marshal(value)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func primeEngineSubscriptionHints(api AdminAPI, rules []Rule, ids []int64) map[string]SubscriptionRef {
	hints := make(map[string]SubscriptionRef)
	for _, id := range ids {
		key := strconv.FormatInt(id, 10)
		for _, rule := range rules {
			if ref, found := rule.SubscriptionRefs[key]; found {
				hints[key] = ref
				break
			}
		}
	}
	if primer, ok := api.(interface {
		PrimeSubscriptionHints(map[string]SubscriptionRef)
	}); ok {
		primer.PrimeSubscriptionHints(hints)
	}
	return hints
}

func actionUsesStoredEvents(state *State, action Action) bool {
	ids := action.EventIDs
	if len(ids) == 0 {
		ids = []string{action.EventID}
	}
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		found := false
		for _, event := range state.Events {
			if event.ID == id {
				found = event.Source == "stored"
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
