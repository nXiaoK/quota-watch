package main

import (
	"errors"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ConfigInput struct {
	Config
	Update             *UpdateConfig `json:"update"`
	ClearAdminAPIKey   bool          `json:"clear_admin_api_key"`
	ClearTelegramToken bool          `json:"clear_telegram_token"`
	ClearTelegramProxy bool          `json:"clear_telegram_proxy"`
	ClearSMTPPassword  bool          `json:"clear_smtp_password"`
}

func mergeConfig(input ConfigInput, previous Config) (Config, error) {
	cfg := input.Config
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/api/v1")
	cfg.AdminAPIKey = strings.TrimSpace(cfg.AdminAPIKey)
	cfg.Telegram.BotToken = strings.TrimSpace(cfg.Telegram.BotToken)
	cfg.Telegram.ChatID = strings.TrimSpace(cfg.Telegram.ChatID)
	cfg.Telegram.ProxyURL = strings.TrimSpace(cfg.Telegram.ProxyURL)
	cfg.Email.Host = strings.TrimSpace(cfg.Email.Host)
	cfg.Email.Username = strings.TrimSpace(cfg.Email.Username)
	cfg.Email.From = strings.TrimSpace(cfg.Email.From)
	if input.Update == nil {
		cfg.Update = previous.Update
	} else {
		cfg.Update = *input.Update
	}
	cfg.Update = normalizeUpdateConfig(cfg.Update)
	if previous.BaseURL != "" && cfg.BaseURL != previous.BaseURL {
		cfg.Update.IdleEnabled = false
		cfg.Update.ScheduledEnabled = false
		cfg.Update.ScheduledForceEnabled = false
		cfg.Update.NotifyAvailableTelegramEnabled = false
		cfg.Update.NotifyTelegramEnabled = false
		cfg.Update.NotifyWindowMissedTelegramEnabled = false
		cfg.Update.NotifyEmailEnabled = false
	}
	if cfg.AdminAPIKey == "" && !input.ClearAdminAPIKey {
		cfg.AdminAPIKey = previous.AdminAPIKey
	}
	if cfg.Telegram.BotToken == "" && !input.ClearTelegramToken {
		cfg.Telegram.BotToken = previous.Telegram.BotToken
	}
	if cfg.Telegram.ProxyURL == "" && !input.ClearTelegramProxy {
		cfg.Telegram.ProxyURL = previous.Telegram.ProxyURL
	}
	if cfg.Telegram.AllowedUserIDs == nil {
		cfg.Telegram.AllowedUserIDs = append([]int64{}, previous.Telegram.AllowedUserIDs...)
	}
	allowedUsers := make([]int64, 0, len(cfg.Telegram.AllowedUserIDs))
	seenUsers := map[int64]bool{}
	for _, id := range cfg.Telegram.AllowedUserIDs {
		if id <= 0 {
			return Config{}, errors.New("Telegram 授权用户 ID 必须为正整数")
		}
		if !seenUsers[id] {
			allowedUsers = append(allowedUsers, id)
			seenUsers[id] = true
		}
	}
	sort.Slice(allowedUsers, func(i, j int) bool { return allowedUsers[i] < allowedUsers[j] })
	cfg.Telegram.AllowedUserIDs = allowedUsers
	if cfg.Email.Password == "" && !input.ClearSMTPPassword {
		cfg.Email.Password = previous.Email.Password
	}
	if input.ClearAdminAPIKey {
		cfg.AdminAPIKey = ""
	}
	if input.ClearTelegramToken {
		cfg.Telegram.BotToken = ""
	}
	if input.ClearTelegramProxy {
		cfg.Telegram.ProxyURL = ""
	}
	if input.ClearSMTPPassword {
		cfg.Email.Password = ""
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, errors.New("主站地址必须是无用户名、查询参数或片段的 HTTP/HTTPS URL")
		}
	}
	if cfg.PollIntervalSeconds < 10 {
		return Config{}, errors.New("检查间隔至少 10 秒")
	}
	if cfg.ConfirmDelaySeconds < 1 || cfg.ConfirmDelaySeconds > 60 {
		return Config{}, errors.New("复核间隔应为 1–60 秒")
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 20 {
		return Config{}, errors.New("并发请求数应为 1–20")
	}
	if strings.ContainsAny(cfg.AdminAPIKey, "\r\n") {
		return Config{}, errors.New("管理员 Key 包含无效字符")
	}
	if (cfg.Update.IdleEnabled || cfg.Update.ScheduledEnabled || cfg.Update.NotifyAvailableTelegramEnabled) && (cfg.BaseURL == "" || cfg.AdminAPIKey == "") {
		return Config{}, errors.New("启用自动更新或新版本通知前请配置 Sub2API 地址和管理员 Key")
	}
	if !validUpdateTime(cfg.Update.WindowStart) || !validUpdateTime(cfg.Update.WindowEnd) || cfg.Update.WindowEnd <= cfg.Update.WindowStart {
		return Config{}, errors.New("自动更新时段必须是同一天内递增的 HH:MM 时间")
	}
	if _, err := time.LoadLocation(cfg.Update.Timezone); err != nil || cfg.Update.Timezone == "Local" {
		return Config{}, errors.New("自动更新时区无效，请填写 IANA 时区，例如 Asia/Shanghai")
	}
	if cfg.Telegram.Enabled && (cfg.Telegram.BotToken == "" || cfg.Telegram.ChatID == "") {
		return Config{}, errors.New("启用 Telegram 时请填写 Token 和 Chat ID")
	}
	if cfg.Update.NotifyWindowMissedTelegramEnabled && !cfg.Telegram.Enabled {
		return Config{}, errors.New("启用时段未更新 Telegram 通知前请先启用 Telegram 通道")
	}
	if cfg.Update.NotifyTelegramEnabled && !cfg.Telegram.Enabled {
		return Config{}, errors.New("启用更新成功 Telegram 通知前请先启用 Telegram 通道")
	}
	if cfg.Update.NotifyAvailableTelegramEnabled && !manualTelegramReady(cfg.Telegram) {
		return Config{}, errors.New("启用新版本确认前请配置 Telegram 私聊，或设置群组授权用户 ID")
	}
	if cfg.Update.NotifyEmailEnabled && !cfg.Email.Enabled {
		return Config{}, errors.New("启用更新成功邮件通知前请先启用邮件通道")
	}
	if cfg.Telegram.ProxyURL != "" {
		u, err := url.Parse(cfg.Telegram.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return Config{}, errors.New("Telegram 代理应为 HTTP/HTTPS/SOCKS5 URL")
		}
	}
	if cfg.Email.TLSMode == "" {
		cfg.Email.TLSMode = "starttls"
	}
	if cfg.Email.TLSMode != "starttls" && cfg.Email.TLSMode != "tls" {
		return Config{}, errors.New("邮件连接请选择 STARTTLS 或 TLS")
	}
	if cfg.Email.Enabled {
		if cfg.Email.Host == "" || strings.ContainsAny(cfg.Email.Host, "\r\n/ ") || cfg.Email.Port < 1 || cfg.Email.Port > 65535 {
			return Config{}, errors.New("请填写有效 SMTP 主机和端口")
		}
		if _, err := mail.ParseAddress(cfg.Email.From); err != nil || strings.ContainsAny(cfg.Email.From, "\r\n") {
			return Config{}, errors.New("发件人地址无效")
		}
		if len(cfg.Email.Recipients) == 0 {
			return Config{}, errors.New("请至少填写一个收件人")
		}
	}
	recipients := []string{}
	seen := map[string]bool{}
	for _, recipient := range cfg.Email.Recipients {
		recipient = strings.TrimSpace(recipient)
		if recipient == "" {
			continue
		}
		if _, err := mail.ParseAddress(recipient); err != nil || strings.ContainsAny(recipient, "\r\n") {
			return Config{}, errors.New("收件人地址无效")
		}
		if !seen[recipient] {
			recipients = append(recipients, recipient)
			seen[recipient] = true
		}
	}
	cfg.Email.Recipients = recipients
	if cfg.Email.Enabled && len(recipients) == 0 {
		return Config{}, errors.New("请至少填写一个收件人")
	}
	return cfg, nil
}

func publicConfig(cfg Config) map[string]any {
	return map[string]any{
		"base_url": cfg.BaseURL, "admin_api_key": "", "admin_api_key_configured": cfg.AdminAPIKey != "",
		"poll_interval_seconds": cfg.PollIntervalSeconds, "confirm_delay_seconds": cfg.ConfirmDelaySeconds,
		"concurrency": cfg.Concurrency, "auto_reset_enabled": cfg.AutoResetEnabled,
		"verbose_logging": cfg.VerboseLogging,
		"update":          normalizeUpdateConfig(cfg.Update),
		"telegram":        map[string]any{"enabled": cfg.Telegram.Enabled, "bot_token": "", "bot_token_configured": cfg.Telegram.BotToken != "", "chat_id": cfg.Telegram.ChatID, "proxy_url": "", "proxy_url_configured": cfg.Telegram.ProxyURL != "", "allowed_user_ids": cfg.Telegram.AllowedUserIDs},
		"email":           map[string]any{"enabled": cfg.Email.Enabled, "host": cfg.Email.Host, "port": cfg.Email.Port, "tls_mode": cfg.Email.TLSMode, "username": cfg.Email.Username, "password": "", "password_configured": cfg.Email.Password != "", "from": cfg.Email.From, "recipients": cfg.Email.Recipients},
	}
}

func normalizeUpdateConfig(cfg UpdateConfig) UpdateConfig {
	defaults := defaultUpdateConfig()
	cfg.WindowStart = strings.TrimSpace(cfg.WindowStart)
	cfg.WindowEnd = strings.TrimSpace(cfg.WindowEnd)
	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	if cfg.WindowStart == "" {
		cfg.WindowStart = defaults.WindowStart
	}
	if cfg.WindowEnd == "" {
		cfg.WindowEnd = defaults.WindowEnd
	}
	if cfg.Timezone == "" {
		cfg.Timezone = defaults.Timezone
	}
	return cfg
}

func validUpdateTime(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	parsed, err := time.Parse("15:04", value)
	return err == nil && parsed.Format("15:04") == value
}

func validateRules(rules []Rule) error {
	seen := map[string]bool{}
	for _, rule := range rules {
		if rule.ID == "" || len(rule.ID) > 120 || seen[rule.ID] {
			return errors.New("规则 ID 缺失、重复或过长")
		}
		seen[rule.ID] = true
		if strings.TrimSpace(rule.Name) == "" {
			return errors.New("请填写规则名称")
		}
		if len(rule.AccountIDs) == 0 {
			return errors.New("每条规则至少选择一个来源账号")
		}
		for _, ids := range [][]int64{rule.AccountIDs, rule.SubscriptionIDs} {
			seenIDs := map[int64]bool{}
			for _, id := range ids {
				if id <= 0 || seenIDs[id] {
					return errors.New("账号或订阅 ID 无效或重复")
				}
				seenIDs[id] = true
			}
		}
		if rule.AutoReset && (len(rule.SubscriptionIDs) == 0 || (!rule.Daily && !rule.Weekly && !rule.Monthly)) {
			return errors.New("自动重置规则请选择目标订阅及重置周期")
		}
	}
	return nil
}
