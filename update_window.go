package main

import (
	"fmt"
	"time"
)

// Keep the last observed window separately from Update.Status: a version check
// or a tick outside the window must not erase the reason it could not update.
// Persisting Reported also prevents repeats after restart or delivery pruning.
type UpdateScheduleWindow struct {
	Day            string    `json:"day"`
	WindowStart    string    `json:"window_start"`
	WindowEnd      string    `json:"window_end"`
	Timezone       string    `json:"timezone"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
	LastObservedAt time.Time `json:"last_observed_at"`
	Version        string    `json:"version,omitempty"`
	HasUpdate      bool      `json:"has_update"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error,omitempty"`
	LastUsageAt    time.Time `json:"last_usage_at,omitempty"`
	Reported       bool      `json:"reported"`
}

func scheduleWindowMatches(w *UpdateScheduleWindow, cfg UpdateConfig) bool {
	return w != nil && cfg.ScheduledEnabled && cfg.NotifyWindowMissedTelegramEnabled &&
		w.WindowStart == cfg.WindowStart && w.WindowEnd == cfg.WindowEnd && w.Timezone == cfg.Timezone
}

// Called before upstream requests, so errors from a new version check cannot
// suppress or overwrite the report for the window that has just ended.
func (u *Updater) syncScheduleWindow(cfg Config, now time.Time) error {
	snapshot, err := u.store.Snapshot()
	if err != nil {
		return err
	}
	if !cfg.Update.ScheduledEnabled || !cfg.Update.NotifyWindowMissedTelegramEnabled {
		if snapshot.Update.ScheduleWindow == nil || snapshot.Update.ScheduleWindow.Reported {
			return nil
		}
		// Discard the active report, but retain the terminal marker so toggling
		// the switch back on cannot report the same window a second time.
		return u.setUpdateState(cfg, func(s *UpdateState) {
			if s.ScheduleWindow != nil {
				s.ScheduleWindow.Reported = true
			}
		})
	}
	inside, day, start, err := updateWindow(now, cfg.Update)
	if err != nil {
		return err
	}
	return u.store.UpdateForConfigWithCurrent(cfg, func(state *State, current Config) error {
		if !sameUpdateTriggerSettings(current.Update, cfg.Update) ||
			!current.Update.NotifyWindowMissedTelegramEnabled {
			return nil
		}
		w := state.Update.ScheduleWindow
		if !scheduleWindowMatches(w, current.Update) {
			w = nil
			state.Update.ScheduleWindow = nil
		}
		if w != nil && !now.Before(w.EndAt) && !w.Reported {
			w.Reported = true
			// Intentionally declined releases and a healthy "no update" check are
			// not failures. A check error is reportable even without a known version.
			if current.Telegram.Enabled && (w.HasUpdate && w.Status != "declined" && !containsString(state.Update.DeclinedVersions, w.Version) || w.Status == "check_failed" || w.Status == "failed") {
				id := "sub2api-update-window-" + w.EndAt.Format("20060102T150405Z")
				alreadyQueued := false
				for _, d := range state.Deliveries {
					if d.ID == id {
						alreadyQueued = true
						break
					}
				}
				if !alreadyQueued {
					state.Deliveries = append(state.Deliveries, Delivery{
						ID: id, Kind: "update_window_missed", Channel: "telegram",
						Message: scheduleWindowMissedMessage(*w, now), Status: "pending",
						NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
					})
				}
			}
		}
		if inside && (w == nil || !w.StartAt.Equal(start)) {
			location, err := time.LoadLocation(current.Update.Timezone)
			if err != nil {
				return err
			}
			clock, err := time.Parse("15:04", current.Update.WindowEnd)
			if err != nil {
				return err
			}
			local := start.In(location)
			end := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
			state.Update.ScheduleWindow = &UpdateScheduleWindow{
				Day: day, WindowStart: current.Update.WindowStart, WindowEnd: current.Update.WindowEnd,
				Timezone: current.Update.Timezone, StartAt: start, EndAt: end.UTC(),
			}
		}
		return nil
	})
}

func (u *Updater) recordScheduleOutcome(cfg Config, tickAt time.Time) error {
	if !cfg.Update.ScheduledEnabled || !cfg.Update.NotifyWindowMissedTelegramEnabled {
		return nil
	}
	return u.store.UpdateForConfigWithCurrent(cfg, func(state *State, current Config) error {
		s := &state.Update
		w := s.ScheduleWindow
		if !scheduleWindowMatches(w, current.Update) || tickAt.Before(w.StartAt) || !tickAt.Before(w.EndAt) || w.Reported {
			return nil
		}
		w.Status, w.LastError = s.Status, s.LastError
		w.Version, w.LastUsageAt = s.LatestVersion, s.LastUsageAt
		w.LastObservedAt = u.now().UTC()
		// checkVersion clears HasUpdate on read errors as a safety precaution;
		// do not lose the previously known queued version in the window summary.
		if s.Status != "check_failed" {
			w.HasUpdate = s.HasUpdate
		}
		return nil
	})
}

func scheduleWindowMissedMessage(w UpdateScheduleWindow, now time.Time) string {
	location, err := time.LoadLocation(w.Timezone)
	if err != nil {
		location = time.UTC
	}
	reason := "尚未满足自动更新条件，将继续按配置等待后续机会。"
	next := "后续将按已启用的空闲或指定时段规则继续检查，仍须连续 10 分钟无使用记录；不会强制更新。"
	switch w.Status {
	case "waiting_idle":
		reason = "最后一次检查时，尚未满足连续 10 分钟无使用记录的空闲条件。"
		if w.LastError != "" {
			reason = "最后一次检查时，读取或复查使用记录失败，无法确认站点是否空闲。"
		}
	case "waiting_engine":
		reason = "最后一次检查时，其他监控或重置任务正在执行，未取得更新执行锁。"
	case "awaiting_approval", "queued":
		if w.Status == "awaiting_approval" {
			reason = "窗口内最后检查时，最新版本仍待 Telegram 确认，未获安装授权。"
		}
	case "approval_unavailable":
		reason = "Telegram 版本确认不可用，请检查通道及授权用户配置。"
	case "check_failed":
		reason = "版本检查失败，无法安全判断或安装新版本。"
	case "failed", "already_attempted":
		reason = "更新尝试失败或该版本当天已尝试，不会在当天重复安装。"
	case "updating", "restarting", "unknown":
		reason = "更新已开始，但尚未完成重启或运行版本核对；这不等于更新失败。"
		next = "程序将继续核对执行结果；结果未知时不会重复提交更新，请查看最近更新状态。"
	}
	if w.LastError != "" {
		reason += "\n错误：" + w.LastError
	}
	message := fmt.Sprintf("Sub2API 指定时段未完成更新\n更新时段：%s %s–%s（%s）", w.Day, w.WindowStart, w.WindowEnd, w.Timezone)
	if w.Version != "" {
		message += "\n待更新版本：" + w.Version
	}
	message += "\n原因：" + reason
	if !w.LastUsageAt.IsZero() {
		message += "\n最后使用记录：" + w.LastUsageAt.In(location).Format("2006-01-02 15:04:05")
	}
	message += "\n窗口内最后检查：" + w.LastObservedAt.In(location).Format("2006-01-02 15:04:05")
	message += "\n通知生成时间：" + now.In(location).Format("2006-01-02 15:04:05")
	return message + "\n" + next
}
