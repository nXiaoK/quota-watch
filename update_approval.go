package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const updateApprovalTTL = 30 * 24 * time.Hour

func newUpdateApproval(cfg Config, version string, now time.Time) (UpdateApproval, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return UpdateApproval{}, err
	}
	return UpdateApproval{
		ID: hex.EncodeToString(raw[:]), Version: version, Status: "pending",
		ChatID: cfg.Telegram.ChatID, ConnectionFingerprint: manualConnectionFingerprint(cfg),
		TelegramFingerprint: manualTelegramFingerprint(cfg.Telegram),
		CreatedAt:           now, ExpiresAt: now.Add(updateApprovalTTL),
	}, nil
}

func updateApprovalScopeValid(cfg Config, state *State, approval UpdateApproval) bool {
	return cfg.Update.NotifyAvailableTelegramEnabled && manualTelegramReady(cfg.Telegram) &&
		validManualRequestID(approval.ID) && approval.Version != "" && state.Update.HasUpdate &&
		approval.Version == state.Update.LatestVersion && approval.ChatID == cfg.Telegram.ChatID &&
		approval.ConnectionFingerprint == manualConnectionFingerprint(cfg) &&
		approval.TelegramFingerprint == manualTelegramFingerprint(cfg.Telegram)
}

func updateApprovalGranted(cfg Config, state *State) bool {
	approval := state.UpdateApproval
	return approval.Status == "approved" && approval.ApprovedBy > 0 && !approval.DecisionAt.IsZero() &&
		!updateVersionDeclined(state) && updateApprovalScopeValid(cfg, state, approval)
}

func updateVersionDeclined(state *State) bool {
	return state.Update.HasUpdate && containsString(state.Update.DeclinedVersions, state.Update.LatestVersion)
}

func cancelUpdateAvailableDeliveries(state *State, now time.Time) {
	for i := range state.Deliveries {
		item := &state.Deliveries[i]
		if item.Kind == "update_available" && (item.Status == "pending" || item.Status == "failed" || item.Status == "running") {
			item.Status, item.UpdatedAt = "cancelled", now
		}
	}
}

func queueUpdateAvailableDelivery(state *State, approval UpdateApproval, now time.Time) error {
	tag, releaseURL, err := sub2apiReleaseURL(approval.Version)
	if err != nil {
		return err
	}
	state.Deliveries = append(state.Deliveries, Delivery{
		ID: "sub2api-update-available-" + approval.ID, Kind: "update_available",
		UpdateRequestID: approval.ID, Channel: "telegram",
		Message: "检测到 Sub2API 新版本：" + tag + "\n版本链接：" + releaseURL + "\n等待 Telegram 确认是否加入空闲更新队列。",
		Status:  "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	})
	return nil
}

func parseUpdateApprovalCallbackData(data string) (string, string, bool) {
	if len(data) != len("qw:ua:")+32 || (!strings.HasPrefix(data, "qw:ua:") && !strings.HasPrefix(data, "qw:ur:")) {
		return "", "", false
	}
	id := data[len("qw:ua:"):]
	if !validManualRequestID(id) {
		return "", "", false
	}
	return data[4:5], id, true
}

// DecideUpdateVersion records one version choice. The callback never starts an
// update: the usual idle/window checks decide when an approved release runs.
func (e *Engine) DecideUpdateVersion(ctx context.Context, callback TelegramCallback) (ManualDecision, error) {
	decision := ManualDecision{Text: "此版本确认请求无效"}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	choice, id, ok := parseUpdateApprovalCallbackData(callback.Data)
	if !ok {
		return decision, nil
	}
	cfg, err := e.store.Config()
	if err != nil {
		return decision, err
	}
	update := func(state *State, current Config) error {
		approval := &state.UpdateApproval
		if approval.ID != id {
			return nil
		}
		if !manualCallbackAuthorized(current.Telegram, ManualRequest{ChatID: approval.ChatID}, callback) {
			decision.Text = "你无权处理此版本确认请求"
			return nil
		}
		decision.RequestID = id
		if approval.Status != "pending" {
			decision.Text = "此版本已处理或已失效"
			return nil
		}
		now := e.Now().UTC()
		if !approval.ExpiresAt.After(now) {
			approval.Status = "expired"
			decision.Text = "此版本确认已过期，等待下次检查重新通知"
			return nil
		}
		if !updateApprovalScopeValid(current, state, *approval) {
			approval.Status = "invalid"
			decision.Text = "版本或配置已改变，等待新的版本通知"
			return nil
		}
		approval.DecisionAt = now
		if choice == "r" {
			approval.Status = "declined"
			if !containsString(state.Update.DeclinedVersions, approval.Version) {
				state.Update.DeclinedVersions = append(state.Update.DeclinedVersions, approval.Version)
			}
			state.Update.Status = "declined"
			decision.Text = "已拒绝此版本，不会自动更新"
			return nil
		}
		approval.Status, approval.ApprovedBy = "approved", callback.FromID
		state.Update.Status = "queued"
		decision.Text = "已加入自动更新队列，满足空闲和时段条件后执行"
		return nil
	}
	if scoped, ok := e.store.(interface {
		UpdateForConfigWithCurrent(Config, func(*State, Config) error) error
	}); ok {
		err = scoped.UpdateForConfigWithCurrent(cfg, update)
	} else {
		err = e.store.Update(func(state *State) error { return update(state, cfg) })
	}
	if err != nil {
		return ManualDecision{Text: "暂时无法保存选择，请稍后重试"}, errors.New("无法保存版本确认选择")
	}
	return decision, nil
}
