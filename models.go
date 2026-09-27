package main

import (
	"context"
	"net/url"
	"time"
)

type Config struct {
	VerboseLogging      bool           `json:"verbose_logging"`
	BaseURL             string         `json:"base_url"`
	AdminAPIKey         string         `json:"admin_api_key"`
	PollIntervalSeconds int            `json:"poll_interval_seconds"`
	ConfirmDelaySeconds int            `json:"confirm_delay_seconds"`
	Concurrency         int            `json:"concurrency"`
	AutoResetEnabled    bool           `json:"auto_reset_enabled"`
	Telegram            TelegramConfig `json:"telegram"`
	Email               EmailConfig    `json:"email"`
}

type TelegramConfig struct {
	AllowedUserIDs []int64 `json:"allowed_user_ids"`
	Enabled        bool    `json:"enabled"`
	BotToken       string  `json:"bot_token"`
	ChatID         string  `json:"chat_id"`
	ProxyURL       string  `json:"proxy_url"`
}

type EmailConfig struct {
	Enabled    bool     `json:"enabled"`
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	TLSMode    string   `json:"tls_mode"`
	Username   string   `json:"username"`
	Password   string   `json:"password"`
	From       string   `json:"from"`
	Recipients []string `json:"recipients"`
}

func DefaultConfig() Config {
	return Config{PollIntervalSeconds: 60, ConfirmDelaySeconds: 5, Concurrency: 3,
		Email: EmailConfig{Port: 587, TLSMode: "starttls", Recipients: []string{}}}
}

type Rule struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	Enabled          bool                       `json:"enabled"`
	AccountIDs       []int64                    `json:"account_ids"`
	SubscriptionIDs  []int64                    `json:"subscription_ids"`
	SubscriptionRefs map[string]SubscriptionRef `json:"subscription_refs,omitempty"`
	AutoReset        bool                       `json:"auto_reset"`
	Daily            bool                       `json:"daily"`
	Weekly           bool                       `json:"weekly"`
	Monthly          bool                       `json:"monthly"`
	NotifyTelegram   bool                       `json:"notify_telegram"`
	NotifyEmail      bool                       `json:"notify_email"`
}

type SubscriptionRef struct {
	UserID  int64 `json:"user_id"`
	GroupID int64 `json:"group_id"`
}

type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Pages    int `json:"pages"`
}

type Account struct {
	storedQuota     *storedQuotaRecord
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	Platform        string         `json:"platform"`
	Type            string         `json:"type"`
	Status          string         `json:"status"`
	GroupIDs        []int64        `json:"group_ids"`
	ParentAccountID *int64         `json:"parent_account_id,omitempty"`
	QuotaDimension  string         `json:"quota_dimension,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
}

type Subscription struct {
	ID                int64              `json:"id"`
	UserID            int64              `json:"user_id"`
	GroupID           int64              `json:"group_id"`
	Status            string             `json:"status"`
	StartsAt          time.Time          `json:"starts_at"`
	ExpiresAt         time.Time          `json:"expires_at"`
	WeeklyWindowStart *time.Time         `json:"weekly_window_start"`
	DailyUsageUSD     float64            `json:"daily_usage_usd"`
	WeeklyUsageUSD    float64            `json:"weekly_usage_usd"`
	MonthlyUsageUSD   float64            `json:"monthly_usage_usd"`
	User              *SubscriptionUser  `json:"user,omitempty"`
	Group             *SubscriptionGroup `json:"group,omitempty"`
}

type SubscriptionUser struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type SubscriptionGroup struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type ConnectionInfo struct {
	Version string `json:"version"`
	Message string `json:"message"`
}

type QuotaSnapshot struct {
	Source      string  `json:"source"`
	AccountID   int64   `json:"account_id"`
	AccountName string  `json:"account_name"`
	Identity    string  `json:"identity"`
	Plan        string  `json:"plan"`
	Dimension   string  `json:"dimension"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at"`
	FetchedAt   int64   `json:"fetched_at"`
}

type Observation struct {
	AccountID       int64          `json:"account_id"`
	AccountName     string         `json:"account_name"`
	Last            *QuotaSnapshot `json:"last,omitempty"`
	PreviousPercent *float64       `json:"previous_percent,omitempty"`
	Status          string         `json:"status"`
	LastError       string         `json:"last_error,omitempty"`
	CheckedAt       time.Time      `json:"checked_at"`
	NextCheckAt     time.Time      `json:"next_check_at"`
	Failures        int            `json:"failures"`
}

type Event struct {
	Source               string    `json:"source"`
	SampledAt            int64     `json:"sampled_at"`
	ID                   string    `json:"id"`
	AccountID            int64     `json:"account_id"`
	AccountName          string    `json:"account_name"`
	Dimension            string    `json:"dimension"`
	PreviousPercent      float64   `json:"previous_percent"`
	UsedPercent          float64   `json:"used_percent"`
	ResetAt              int64     `json:"reset_at"`
	DetectedAt           time.Time `json:"detected_at"`
	RuleNames            []string  `json:"rule_names"`
	NotificationChannels []string  `json:"notification_channels,omitempty"`
}

type ResetMask struct {
	Daily   bool `json:"daily"`
	Weekly  bool `json:"weekly"`
	Monthly bool `json:"monthly"`
}

type ResetItem struct {
	SubscriptionID int64  `json:"subscription_id"`
	Success        bool   `json:"success"`
	Error          string `json:"error,omitempty"`
}

type ResetResult struct {
	SuccessCount int         `json:"success_count"`
	FailedCount  int         `json:"failed_count"`
	Results      []ResetItem `json:"results"`
}

type Action struct {
	Mode                 string      `json:"mode,omitempty"`
	ManualRequestID      string      `json:"manual_request_id,omitempty"`
	ID                   string      `json:"id"`
	EventID              string      `json:"event_id"`
	EventIDs             []string    `json:"event_ids,omitempty"`
	RuleIDs              []string    `json:"rule_ids"`
	SubscriptionIDs      []int64     `json:"subscription_ids"`
	Mask                 ResetMask   `json:"mask"`
	IdempotencyKey       string      `json:"idempotency_key"`
	NotificationChannels []string    `json:"notification_channels,omitempty"`
	Status               string      `json:"status"`
	Results              []ResetItem `json:"results"`
	LastError            string      `json:"last_error,omitempty"`
	Attempts             int         `json:"attempts"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

type Delivery struct {
	ManualRequestID string    `json:"manual_request_id,omitempty"`
	ID              string    `json:"id"`
	EventID         string    `json:"event_id"`
	Channel         string    `json:"channel"`
	Message         string    `json:"message"`
	Status          string    `json:"status"`
	Attempts        int       `json:"attempts"`
	LastError       string    `json:"last_error,omitempty"`
	NextAttemptAt   time.Time `json:"next_attempt_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Health struct {
	LastCheckAt   time.Time `json:"last_check_at"`
	LastSuccessAt time.Time `json:"last_success_at"`
	LastError     string    `json:"last_error,omitempty"`
	Version       string    `json:"version"`
}

type State struct {
	ManualRequests   []ManualRequest        `json:"manual_requests"`
	TelegramReceiver TelegramReceiverState  `json:"telegram_receiver"`
	Rules            []Rule                 `json:"rules"`
	Observations     map[string]Observation `json:"observations"`
	Events           []Event                `json:"events"`
	Actions          []Action               `json:"actions"`
	Deliveries       []Delivery             `json:"deliveries"`
	Health           Health                 `json:"health"`
}

type ManualTarget struct {
	SubscriptionID int64           `json:"subscription_id"`
	Mask           ResetMask       `json:"mask"`
	Ref            SubscriptionRef `json:"ref"`
}

type ManualRequest struct {
	ID                    string         `json:"id"`
	EventIDs              []string       `json:"event_ids"`
	RuleIDs               []string       `json:"rule_ids"`
	Targets               []ManualTarget `json:"targets"`
	ConnectionFingerprint string         `json:"connection_fingerprint"`
	RulesFingerprint      string         `json:"rules_fingerprint"`
	TelegramFingerprint   string         `json:"telegram_fingerprint"`
	ChatID                string         `json:"chat_id"`
	Status                string         `json:"status"`
	ApprovedBy            int64          `json:"approved_by,omitempty"`
	DecisionAt            time.Time      `json:"decision_at"`
	ActionIDs             []string       `json:"action_ids"`
	CreatedAt             time.Time      `json:"created_at"`
	ExpiresAt             time.Time      `json:"expires_at"`
	LastError             string         `json:"last_error,omitempty"`
}

type TelegramReceiverState struct {
	BotFingerprint string    `json:"bot_fingerprint"`
	Offset         int64     `json:"offset"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

type Simulation struct {
	Rule          Rule           `json:"rule"`
	Accounts      []Account      `json:"accounts"`
	Subscriptions []Subscription `json:"subscriptions"`
	Skipped       []ResetItem    `json:"skipped"`
	WillAutoReset bool           `json:"will_auto_reset"`
	Message       string         `json:"message"`
}

type AdminAPI interface {
	Connection(context.Context) (ConnectionInfo, error)
	ListAccounts(context.Context, url.Values) (Page[Account], error)
	ListSubscriptions(context.Context, url.Values) (Page[Subscription], error)
	GetAccount(context.Context, int64) (Account, error)
	ReadStoredQuota(Account) (QuotaSnapshot, error)
	GetSubscriptions(context.Context, []int64) (map[int64]Subscription, error)
	ResetSubscriptions(context.Context, []int64, ResetMask, string) (ResetResult, error)
}
