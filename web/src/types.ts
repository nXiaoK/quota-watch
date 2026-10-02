export interface Config {
  base_url: string
  admin_api_key: string
  admin_api_key_configured?: boolean
  clear_admin_api_key?: boolean
  clear_telegram_token?: boolean
  clear_telegram_proxy?: boolean
  clear_smtp_password?: boolean
  poll_interval_seconds: number
  confirm_delay_seconds: number
  concurrency: number
  auto_reset_enabled: boolean
  verbose_logging: boolean
  update: UpdateConfig
  telegram: {
    enabled: boolean
    bot_token: string
    bot_token_configured?: boolean
    chat_id: string
    allowed_user_ids: number[] | null
    proxy_url: string
    proxy_url_configured?: boolean
  }
  email: {
    enabled: boolean
    host: string
    port: number
    tls_mode: string
    username: string
    password: string
    password_configured?: boolean
    from: string
    recipients: string[]
  }
}

export interface UpdateConfig {
  notify_window_missed_telegram_enabled: boolean
  idle_enabled: boolean
  scheduled_enabled: boolean
  scheduled_force_enabled: boolean
  notify_available_telegram_enabled: boolean
  notify_telegram_enabled: boolean
  notify_email_enabled: boolean
  window_start: string
  window_end: string
  timezone: string
}

export interface UpdateState {
  status: string
  current_version?: string
  latest_version?: string
  last_check_at?: string
  last_attempt_at?: string
  last_success_at?: string
  last_error?: string
  last_trigger?: string
}

export interface UpdateApproval {
  version: string
  status: string
}

export interface Rule {
  id: string
  name: string
  enabled: boolean
  account_ids: number[]
  subscription_ids: number[]
  auto_reset: boolean
  daily: boolean
  weekly: boolean
  monthly: boolean
  notify_telegram: boolean
  notify_email: boolean
}

export interface Page<T> {
  items: T[]
  total: number
  page: number
  page_size: number
  pages: number
}

export interface Account {
  id: number
  name: string
  platform: string
  type: string
  status: string
  group_ids: number[]
  parent_account_id?: number | null
  quota_dimension?: string
}

export interface Subscription {
  id: number
  user_id: number
  group_id: number
  status: string
  starts_at: string
  expires_at: string
  weekly_window_start?: string | null
  daily_usage_usd: number
  weekly_usage_usd: number
  monthly_usage_usd: number
  user?: { id: number; email: string; username: string }
  group?: { id: number; name: string; platform: string }
}

export interface QuotaSnapshot {
  account_id: number
  account_name: string
  identity: string
  plan: string
  dimension: string
  used_percent: number
  reset_at: number
  fetched_at: number
  source?: string
}

export interface Observation {
  account_id: number
  account_name: string
  last?: QuotaSnapshot | null
  previous_percent?: number | null
  status: string
  last_error?: string
  checked_at: string
  next_check_at: string
  failures: number
}

export interface Event {
  id: string
  source?: string
  sampled_at?: number
  account_id: number
  account_name: string
  dimension: string
  previous_percent: number
  used_percent: number
  reset_at: number
  detected_at: string
  rule_names: string[]
  notification_channels?: string[]
}

export interface ResetItem {
  subscription_id: number
  success: boolean
  error?: string
}

export interface Action {
  mode?: string
  manual_request_id?: string
  id: string
  event_id: string
  event_ids?: string[]
  rule_ids: string[]
  subscription_ids: number[]
  mask: { daily: boolean; weekly: boolean; monthly: boolean }
  idempotency_key: string
  status: string
  results: ResetItem[]
  last_error?: string
  attempts: number
  created_at: string
  updated_at: string
}

export interface Delivery {
  manual_request_id?: string
  id: string
  event_id: string
  channel: string
  message: string
  status: string
  attempts: number
  last_error?: string
  next_attempt_at: string
  created_at: string
  updated_at: string
}

export interface ManualRequest {
  id: string
  event_ids: string[]
  rule_ids: string[]
  targets: {
    subscription_id: number
    mask: { daily: boolean; weekly: boolean; monthly: boolean }
    ref: { user_id: number; group_id: number }
  }[]
  chat_id: string
  status: string
  approved_by?: number
  decision_at: string
  action_ids: string[]
  created_at: string
  expires_at: string
  last_error?: string
}

export interface TelegramReceiverState {
  status: string
  last_error?: string
  checked_at: string
  offset: number
  bot_fingerprint: string
}

export interface State {
  manual_requests: ManualRequest[]
  telegram_receiver: TelegramReceiverState
  rules: Rule[]
  observations: Record<string, Observation>
  events: Event[]
  actions: Action[]
  deliveries: Delivery[]
  health: {
    last_check_at: string
    last_success_at: string
    last_error?: string
    version: string
  }
  update: UpdateState
  update_approval?: UpdateApproval
  busy: boolean
}

export interface Simulation {
  rule: Rule
  accounts: Account[]
  subscriptions: Subscription[]
  skipped: ResetItem[]
  will_auto_reset: boolean
  message: string
}
