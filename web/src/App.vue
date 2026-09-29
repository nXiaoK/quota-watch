<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, toRaw } from 'vue'
import { ApiError, cancelSessionRequests, onAuthenticationRequired, request } from './api'
import { dateTime, maskText, percent, statusText } from './format'
import type { Account, Action, Config, Observation, Rule, Simulation, State } from './types'
import AccountPicker from './components/AccountPicker.vue'
import EventHistory from './components/EventHistory.vue'
import LoginPage from './components/LoginPage.vue'
import RuleEditor from './components/RuleEditor.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import ThemeSwitch from './components/ThemeSwitch.vue'
import UiIcon from './components/UiIcon.vue'

type View = 'overview' | 'accounts' | 'rules' | 'history' | 'settings'
type Session = { authenticated: boolean; username: string; expires_at: string }
const navigation: { id: View; label: string; icon: string; description: string }[] = [
  { id: 'overview', label: '概览', icon: 'grid', description: '查看主站已保存的 7d 快照、检查状态与最近重置。' },
  { id: 'accounts', label: '监控账号', icon: 'users', description: '勾选一个或多个账号，创建按任一账号重置触发的规则。' },
  { id: 'rules', label: '联动规则', icon: 'rules', description: '将账号重置事件关联到通知渠道与固定订阅集合。' },
  { id: 'history', label: '事件历史', icon: 'history', description: '回看重置事件、订阅执行结果与通知发送记录。' },
  { id: 'settings', label: '连接设置', icon: 'settings', description: '配置源站、检测频率、全局开关与通知渠道。' },
]
const view = ref<View>('overview')
const pageInfo = computed(() => navigation.find((item) => item.id === view.value)!)
const emptyState: State = { rules: [], observations: {}, events: [], actions: [], deliveries: [], manual_requests: [], telegram_receiver: { status: '', checked_at: '', offset: 0, bot_fingerprint: '' }, health: { last_check_at: '', last_success_at: '', version: '' }, update: { status: '' }, busy: false }
const emptyConfig: Config = { base_url: '', admin_api_key: '', poll_interval_seconds: 60, confirm_delay_seconds: 5, concurrency: 3, auto_reset_enabled: false, verbose_logging: false, update: { idle_enabled: false, scheduled_enabled: false, window_start: '02:00', window_end: '03:00', timezone: 'Asia/Shanghai' }, telegram: { enabled: false, bot_token: '', chat_id: '', allowed_user_ids: [], proxy_url: '' }, email: { enabled: false, host: '', port: 587, tls_mode: 'starttls', username: '', password: '', from: '', recipients: [] } }
const state = ref<State>(structuredClone(emptyState))
const config = ref<Config>(structuredClone(emptyConfig))
const authState = ref<'checking' | 'anonymous' | 'authenticated'>('checking')
const session = ref<Session | null>(null)
const authPending = ref(false)
const authError = ref('')
const loggingOut = ref(false)
const ready = ref(false)
const initialLoading = ref(true)
const stateError = ref('')
const configError = ref('')
const updateAcknowledgeError = ref('')
const notice = ref<{ message: string; error: boolean } | null>(null)
const pending = ref(new Set<string>())
const selectedAccounts = ref<number[]>([])
const accountNames = ref<Record<number, string>>({})
const editor = ref<Rule | null>(null)
const editorIsNew = ref(false)
const simulation = ref<Simulation | null>(null)
const enabledRules = computed(() => state.value.rules.filter((rule) => rule.enabled))
const watchedIds = computed(() => [...new Set(enabledRules.value.flatMap((rule) => rule.account_ids))])
const watchedObservations = computed<Observation[]>(() => watchedIds.value.map((id) => state.value.observations[String(id)] ?? { account_id: id, account_name: accountNames.value[id] || `账号 #${id}`, status: 'initial', checked_at: '', next_check_at: '', failures: 0 }).sort((a, b) => Number(Boolean(b.last_error)) - Number(Boolean(a.last_error)) || a.account_id - b.account_id))
const observationErrors = computed(() => watchedObservations.value.filter((item) => item.status === 'error').length)
const latestEvents = computed(() => [...state.value.events].sort((a, b) => Date.parse(b.detected_at) - Date.parse(a.detected_at)).slice(0, 4))
const retrying = computed(() => [...pending.value].filter((key) => key.startsWith('retry:')).map((key) => key.slice(6)))
const connectionReady = computed(() => Boolean(config.value.base_url && config.value.admin_api_key_configured))
let timer: ReturnType<typeof setInterval> | undefined
let stateRequest = 0
let initializeRequest = 0
let sessionEpoch = 0
let authRequest = 0
let stopObservingAuth: (() => void) | undefined
let alive = true

function sessionCurrent(epoch: number) {
  return alive && authState.value === 'authenticated' && !loggingOut.value && epoch === sessionEpoch
}

function stopPolling() {
  if (timer) clearInterval(timer)
  timer = undefined
}

function startPolling() {
  stopPolling()
  timer = setInterval(() => { if (!document.hidden) void refreshState() }, 15_000)
}

function leaveSession(message = '') {
  sessionEpoch += 1
  authRequest += 1
  stateRequest += 1
  initializeRequest += 1
  stopPolling()
  cancelSessionRequests()
  authState.value = 'anonymous'
  session.value = null
  authPending.value = false
  loggingOut.value = false
  authError.value = message
  state.value = structuredClone(emptyState)
  config.value = structuredClone(emptyConfig)
  ready.value = false
  initialLoading.value = false
  stateError.value = ''
  configError.value = ''
  updateAcknowledgeError.value = ''
  notice.value = null
  pending.value = new Set()
  selectedAccounts.value = []
  accountNames.value = {}
  editor.value = null
  editorIsNew.value = false
  simulation.value = null
  view.value = 'overview'
}

async function enterSession(value: Session) {
  if (!value.authenticated) throw new Error('登录状态无效，请重新登录。')
  sessionEpoch += 1
  cancelSessionRequests()
  session.value = value
  authState.value = 'authenticated'
  authError.value = ''
  const epoch = sessionEpoch
  await initialize()
  if (sessionCurrent(epoch)) startPolling()
}

async function checkSession() {
  const sequence = ++authRequest
  try {
    const result = await request<Session>('/api/auth/session')
    if (!alive || sequence !== authRequest) return
    await enterSession(result)
  } catch (cause) {
    if (!alive || sequence !== authRequest) return
    authState.value = 'anonymous'
    authError.value = cause instanceof ApiError && cause.status === 401 ? '' : cause instanceof Error ? cause.message : '无法连接监控服务，请稍后重试。'
  }
}

async function login(credentials: { username: string; password: string }) {
  if (authPending.value || authState.value !== 'anonymous') return
  const sequence = ++authRequest
  authPending.value = true
  authError.value = ''
  try {
    const result = await request<Session>('/api/auth/login', { method: 'POST', body: credentials })
    if (!alive || sequence !== authRequest) return
    await enterSession(result)
  } catch (cause) {
    if (alive && sequence === authRequest) authError.value = cause instanceof Error ? cause.message : '登录失败，请稍后重试。'
  } finally {
    if (alive && sequence === authRequest) authPending.value = false
  }
}

async function logout() {
  if (loggingOut.value || authState.value !== 'authenticated') return
  loggingOut.value = true
  sessionEpoch += 1
  const epoch = sessionEpoch
  stopPolling()
  cancelSessionRequests()
  try {
    await request('/api/auth/logout', { method: 'POST', body: {} })
    if (alive && epoch === sessionEpoch) leaveSession()
  } catch (cause) {
    if (!alive || epoch !== sessionEpoch) return
    if (cause instanceof ApiError && cause.status === 401) leaveSession()
    else {
      loggingOut.value = false
      pending.value = new Set()
      notice.value = { message: cause instanceof Error ? cause.message : '退出失败，请重试。', error: true }
      await initialize()
      if (sessionCurrent(epoch)) startPolling()
    }
  }
}

function normalizeState(value: State): State {
  return { ...value, rules: value.rules ?? [], observations: value.observations ?? {}, events: value.events ?? [], actions: value.actions ?? [], deliveries: value.deliveries ?? [], manual_requests: value.manual_requests ?? [], telegram_receiver: value.telegram_receiver ?? { status: '', checked_at: '', offset: 0, bot_fingerprint: '' }, health: value.health ?? { last_check_at: '', last_success_at: '', version: '' }, update: value.update ?? { status: '' } }
}

function acceptConfig(value: Config) {
  config.value = { ...value, verbose_logging: value.verbose_logging ?? false, update: { ...emptyConfig.update, ...(value.update ?? {}) }, admin_api_key: '', telegram: { ...value.telegram, bot_token: '', proxy_url: '', allowed_user_ids: value.telegram.allowed_user_ids ?? [] }, email: { ...value.email, password: '', recipients: value.email.recipients ?? [] } }
  ready.value = true
  configError.value = ''
}

async function refreshState() {
  const epoch = sessionEpoch
  if (!sessionCurrent(epoch)) return
  const sequence = ++stateRequest
  try {
    const result = await request<State>('/api/state')
    if (!sessionCurrent(epoch) || sequence !== stateRequest) return
    state.value = normalizeState(result)
    stateError.value = ''
  } catch (cause) {
    if (sessionCurrent(epoch) && sequence === stateRequest) stateError.value = cause instanceof Error ? cause.message : '无法读取监控状态'
  }
}

async function initialize() {
  const epoch = sessionEpoch
  if (!sessionCurrent(epoch)) return
  const sequence = ++initializeRequest
  const stateSequence = ++stateRequest
  initialLoading.value = true
  const results = await Promise.allSettled([request<Config>('/api/config'), request<State>('/api/state')])
  if (!sessionCurrent(epoch) || sequence !== initializeRequest) return
  if (results[0].status === 'fulfilled') acceptConfig(results[0].value)
  else configError.value = results[0].reason instanceof Error ? results[0].reason.message : '配置读取失败'
  if (stateSequence === stateRequest && results[1].status === 'fulfilled') {
    state.value = normalizeState(results[1].value)
    stateError.value = ''
  } else if (stateSequence === stateRequest && results[1].status === 'rejected') stateError.value = results[1].reason instanceof Error ? results[1].reason.message : '状态读取失败'
  initialLoading.value = false
}

async function perform(key: string, action: (ensureActive: () => void) => Promise<void>) {
  const epoch = sessionEpoch
  if (!sessionCurrent(epoch) || pending.value.has(key)) return
  const ensureActive = () => { if (!sessionCurrent(epoch)) throw new DOMException('会话请求已取消', 'AbortError') }
  pending.value.add(key)
  notice.value = null
  try {
    await action(ensureActive)
  } catch (cause) {
    if (sessionCurrent(epoch)) notice.value = { message: cause instanceof Error ? cause.message : '操作失败，请稍后重试。', error: true }
  } finally {
    if (sessionCurrent(epoch)) pending.value.delete(key)
  }
}

function rememberAccounts(accounts: Account[]) {
  for (const account of accounts) accountNames.value[account.id] = account.name || `账号 #${account.id}`
}

function newRule(accountIds: number[] = []) {
  const token = [...crypto.getRandomValues(new Uint32Array(4))].map((value) => value.toString(16).padStart(8, '0')).join('')
  editor.value = { id: token, name: '', enabled: true, account_ids: [...accountIds], subscription_ids: [], auto_reset: false, daily: false, weekly: true, monthly: false, notify_telegram: config.value.telegram.enabled, notify_email: config.value.email.enabled }
  editorIsNew.value = true
  simulation.value = null
  view.value = 'rules'
}

function editRule(rule: Rule) {
  editor.value = structuredClone(toRaw(rule))
  editorIsNew.value = false
  simulation.value = null
  view.value = 'rules'
}

async function saveRule(rule: Rule) {
  await perform('rules', async (ensureActive) => {
    const latest = normalizeState(await request<State>('/api/state'))
    ensureActive()
    const rules = latest.rules.some((item) => item.id === rule.id) ? latest.rules.map((item) => item.id === rule.id ? rule : item) : [...latest.rules, rule]
    await request('/api/rules', { method: 'PUT', body: { rules } })
    ensureActive()
    editor.value = null
    simulation.value = null
    selectedAccounts.value = []
    await refreshState()
    ensureActive()
    notice.value = { message: '规则已保存。首个有效主站快照建立基线，之后检查快照中的重置变化。', error: false }
  })
}

async function toggleRule(rule: Rule) {
  await perform('rules', async (ensureActive) => {
    const latest = normalizeState(await request<State>('/api/state'))
    ensureActive()
    const rules = latest.rules.map((item) => item.id === rule.id ? { ...item, enabled: !rule.enabled } : item)
    await request('/api/rules', { method: 'PUT', body: { rules } })
    ensureActive()
    await refreshState()
    ensureActive()
    notice.value = { message: `规则“${rule.name}”已${rule.enabled ? '停用' : '启用'}。`, error: false }
  })
}

async function deleteRule(rule: Rule) {
  await perform('rules', async (ensureActive) => {
    const latest = normalizeState(await request<State>('/api/state'))
    ensureActive()
    await request('/api/rules', { method: 'PUT', body: { rules: latest.rules.filter((item) => item.id !== rule.id) } })
    ensureActive()
    if (editor.value?.id === rule.id) editor.value = null
    await refreshState()
    ensureActive()
    notice.value = { message: `规则“${rule.name}”已删除，历史记录仍可查询。`, error: false }
  })
}

async function simulateRule(rule: Rule) {
  await perform('simulate', async (ensureActive) => {
    const result = await request<Simulation>('/api/rules/simulate', { method: 'POST', body: { rule } })
    ensureActive()
    simulation.value = result
  })
}

async function saveConfig(value: Config) {
  await perform('config', async (ensureActive) => {
    const sourceChanged = value.base_url.trim().replace(/\/+$/, '') !== config.value.base_url.trim().replace(/\/+$/, '')
    const result = await request<Config>('/api/config', { method: 'PUT', body: value })
    ensureActive()
    acceptConfig(result)
    if (sourceChanged) {
      editor.value = null
      simulation.value = null
      selectedAccounts.value = []
      accountNames.value = {}
    }
    await refreshState()
    ensureActive()
    notice.value = { message: sourceChanged ? '设置已保存。源站已切换，请重新核对并启用联动规则。' : '设置已保存，敏感输入已清空。', error: false }
  })
}

async function acknowledgeUpdate() {
  const epoch = sessionEpoch
  const key = 'update-acknowledge'
  if (!sessionCurrent(epoch) || pending.value.has(key) || state.value.update.status !== 'unknown') return
  updateAcknowledgeError.value = ''
  pending.value.add(key)
  let acknowledged = false
  try {
    await request<{ message?: string }>('/api/update/acknowledge', { method: 'POST' })
    acknowledged = true
    if (!sessionCurrent(epoch)) return
    const sequence = ++stateRequest
    const result = await request<State>('/api/state')
    if (!sessionCurrent(epoch)) return
    if (sequence === stateRequest) {
      state.value = normalizeState(result)
      stateError.value = ''
    }
  } catch (cause) {
    if (sessionCurrent(epoch)) {
      const message = cause instanceof Error ? cause.message : '请稍后重试。'
      updateAcknowledgeError.value = acknowledged ? `已确认，但刷新状态失败：${message}` : message
    }
  } finally {
    if (sessionCurrent(epoch)) pending.value.delete(key)
  }
}

async function testConnection(value: Config) {
  await perform('connection', async (ensureActive) => {
    const result = await request<{ version?: string; message: string }>('/api/connection/test', { method: 'POST', body: value })
    ensureActive()
    notice.value = { message: `连接成功${result.version ? ` · Sub2API ${result.version}` : ''}。${result.message || ''}`, error: false }
  })
}

async function testNotification(channel: 'telegram' | 'email', value: Config) {
  await perform(channel, async (ensureActive) => {
    const result = await request<{ message: string }>('/api/notifications/test', { method: 'POST', body: { channel, config: value } })
    ensureActive()
    notice.value = { message: result.message || '测试通知已发送。', error: false }
  })
}

async function checkNow() {
  await perform('check', async (ensureActive) => {
    const result = await request<{ message: string }>('/api/check', { method: 'POST', body: {} })
    ensureActive()
    notice.value = { message: result.message, error: false }
    await refreshState()
  })
}

async function retryAction(action: Action) {
  await perform(`retry:${action.id}`, async (ensureActive) => {
    const result = await request<{ message: string }>(`/api/actions/${encodeURIComponent(action.id)}/retry`, { method: 'POST', body: {} })
    ensureActive()
    await refreshState()
    ensureActive()
    notice.value = { message: result.message || '失败项重试已处理。', error: false }
  })
}

onMounted(() => {
  stopObservingAuth = onAuthenticationRequired(() => leaveSession('登录已过期，请重新登录。'))
  void checkSession()
})
onBeforeUnmount(() => { alive = false; stopPolling(); cancelSessionRequests(); stopObservingAuth?.() })
</script>

<template>
  <LoginPage v-if="authState !== 'authenticated'" :checking="authState === 'checking'" :busy="authPending" :error="authError" @login="login" />
  <div v-else class="app-shell">
    <a class="skip-link" href="#main-content">跳到主要内容</a>
    <aside class="sidebar">
      <a href="#" class="brand" aria-label="Quota Watch 概览" @click.prevent="view = 'overview'"><span class="brand-symbol"><UiIcon name="pulse" :size="25" /></span><span>Quota Watch<small>额度监控 · 订阅联动</small></span></a>
      <div class="sidebar-label">工作台</div>
      <nav aria-label="主导航"><button v-for="item in navigation" :key="item.id" type="button" :class="{ active: view === item.id }" :aria-current="view === item.id ? 'page' : undefined" @click="view = item.id"><UiIcon :name="item.icon" :size="19" /><span>{{ item.label }}</span><span v-if="item.id === 'rules' && state.rules.length" class="nav-count mono">{{ state.rules.length }}</span></button></nav>
      <div class="sidebar-bottom"><div class="service-status"><span class="status-dot" :class="{ live: !stateError && ready }" /><span>{{ stateError ? '状态连接异常' : initialLoading ? '连接服务中' : '监控服务运行中' }}</span></div><p>独立运行 · 通过管理员 API 联动</p></div>
    </aside>
    <div class="main-shell">
      <header class="topbar"><span class="topbar-breadcrumb">工作台 <span>/</span> {{ pageInfo.label }}</span><div class="topbar-tools"><div class="topbar-status"><span class="status-pill" :class="{ success: config.auto_reset_enabled }"><UiIcon name="shield" :size="13" /> 自动重置{{ config.auto_reset_enabled ? '已开启' : '关闭' }}</span><span class="mono muted caption">{{ config.poll_interval_seconds }}s / 轮</span></div><ThemeSwitch /><span class="session-user caption muted" :title="session?.username">{{ session?.username }}</span><button class="button small secondary logout-button" type="button" :disabled="loggingOut" @click="logout"><UiIcon name="logout" :size="15" /> {{ loggingOut ? '正在退出…' : '退出' }}</button></div></header>
      <main id="main-content" tabindex="-1">
        <div class="page-heading"><div><div class="eyebrow">QUOTA AUTOMATION</div><h1>{{ pageInfo.label }}</h1><p>{{ pageInfo.description }}</p></div><div class="page-actions"><button class="button secondary" type="button" :disabled="initialLoading || pending.has('check') || state.busy || !connectionReady || !watchedIds.length" @click="checkNow"><UiIcon name="refresh" :size="17" :class="{ spinning: state.busy }" /> {{ state.busy ? '快照检查中' : pending.has('check') ? '正在安排…' : '立即检查快照' }}</button><button v-if="view === 'overview' || view === 'rules'" class="button primary" type="button" :disabled="!ready || !connectionReady" @click="newRule()"><UiIcon name="plus" :size="18" /> 新建规则</button><button v-else class="button icon-button" type="button" aria-label="刷新监控状态" :disabled="initialLoading" @click="refreshState"><UiIcon name="refresh" /></button></div></div>
        <div v-if="notice" class="notice" :class="{ error: notice.error }" :role="notice.error ? 'alert' : 'status'"><UiIcon :name="notice.error ? 'shield' : 'check'" :size="18" /><span>{{ notice.message }}</span><button class="text-button" type="button" aria-label="关闭提示" @click="notice = null"><UiIcon name="close" :size="17" /></button></div>
        <div v-if="stateError" class="inline-message error" role="alert"><span>状态读取失败：{{ stateError }}。以下显示最近一次已读取的数据。</span><button class="text-button" type="button" @click="refreshState">重新读取</button></div>
        <div v-if="configError" class="inline-message error" role="alert"><span>配置读取失败：{{ configError }}</span><button class="text-button" type="button" @click="initialize">重新连接</button></div>
        <div v-if="initialLoading" class="panel empty-state"><UiIcon name="refresh" class="spinning" :size="30" /><h2>正在连接监控服务</h2><p>读取配置与最近检测状态。</p></div>
        <template v-else>
          <section v-if="ready && !connectionReady && view !== 'settings'" class="setup-banner"><span class="section-icon"><UiIcon name="link" /></span><div><h2>先连接你的 Sub2API</h2><p>配置站点地址与管理员 API Key 后，即可勾选账号、设置通知与订阅联动。</p></div><button class="button primary" type="button" @click="view = 'settings'">连接设置 <UiIcon name="arrow" :size="16" /></button></section>
          <div v-if="view === 'overview'" class="overview">
            <div class="metric-grid"><article class="metric-card"><span>监控账号</span><strong class="mono">{{ watchedIds.length }}<small>个</small></strong><p>{{ enabledRules.length }} 条启用规则</p><UiIcon name="users" /></article><article class="metric-card"><span>重置事件</span><strong class="mono">{{ state.events.length }}<small>次</small></strong><p>{{ latestEvents.length ? `最近 ${dateTime(latestEvents[0].detected_at)}` : '等待首次重置事件' }}</p><UiIcon name="history" /></article><article class="metric-card"><span>订阅执行</span><strong class="mono">{{ state.actions.filter((action) => action.status === 'succeeded').length }}<small>次成功</small></strong><p>{{ state.actions.filter((action) => ['failed', 'partial', 'unknown'].includes(action.status)).length }} 次待处理</p><UiIcon name="refresh" /></article><article class="metric-card"><span>检测异常</span><strong class="mono" :class="{ 'error-text': observationErrors }">{{ observationErrors }}<small>个账号</small></strong><p>{{ state.health.last_success_at && !state.health.last_success_at.startsWith('0001-') ? `上次成功 ${dateTime(state.health.last_success_at)}` : '尚无成功检测记录' }}</p><UiIcon name="pulse" /></article></div>
            <div class="overview-grid">
              <section class="panel monitor-panel"><div class="panel-heading"><div><h2>7d 使用状态</h2><p class="muted">主站已有快照 · 每 {{ config.poll_interval_seconds }} 秒检查 · 不主动查询 OpenAI</p></div><span class="status-pill" :class="{ success: state.busy }"><span class="status-dot" :class="{ live: state.busy }" /> {{ state.busy ? '检查中' : '等待下一轮' }}</span></div>
                <div v-if="state.health.last_error" class="inline-message error panel-error" role="alert">{{ state.health.last_error }}</div>
                <div v-if="watchedObservations.length" class="table-scroll"><table><thead><tr><th>账号</th><th>7d 使用率 / 主站采样</th><th>状态 / 快照记录的重置时间</th></tr></thead><tbody><tr v-for="observation in watchedObservations" :key="observation.account_id"><td><div class="table-primary">{{ observation.account_name }}</div><span class="caption muted mono">#{{ observation.account_id }}<span v-if="observation.last"> · {{ observation.last.dimension === 'spark' ? 'Spark' : '普通 Codex' }}</span></span><div class="caption muted">本服务检查 {{ dateTime(observation.checked_at) }}</div><div v-if="observation.last" class="caption muted">来源：{{ observation.last.source === 'stored' ? '主站已有快照（stored）' : '未知' }}</div></td><td><div class="quota-value mono">{{ percent(observation.last?.used_percent) }}<span v-if="['waiting', 'unknown', 'error'].includes(observation.status) && observation.last" class="caption muted">最近有效值</span></div><div v-if="observation.last && Number.isFinite(observation.last.used_percent)" class="quota-track" :class="{ stale: ['waiting', 'unknown', 'error'].includes(observation.status) }"><span :style="{ width: `${Math.min(100, Math.max(0, observation.last.used_percent))}%` }" /></div><div v-if="observation.last" class="caption muted">主站采样 {{ dateTime(observation.last.fetched_at) }}</div></td><td><span class="status-pill" :class="{ error: observation.status === 'error', success: ['watching', 'baseline', 'reset'].includes(observation.status) && observation.last }">{{ statusText(observation.status) }}</span><div v-if="observation.last_error" class="caption observation-error" :class="observation.status === 'error' ? 'error-text' : 'muted'">{{ observation.last_error }}</div><div v-else class="caption muted">{{ dateTime(observation.last?.reset_at) }}</div></td></tr></tbody></table></div>
                <div v-else class="empty-state"><UiIcon name="users" :size="30" /><h3>还没有正在监控的账号</h3><p>从账号列表勾选账号并保存启用规则，开始建立主站快照基线。</p><button class="button secondary" type="button" @click="view = 'accounts'">选择监控账号 <UiIcon name="arrow" :size="16" /></button></div>
                <div class="panel-footnote">上次快照检查 {{ dateTime(state.health.last_check_at) }}<span v-if="state.health.version"> · Sub2API {{ state.health.version }}</span></div>
              </section>
              <section class="panel mechanism-panel"><div class="panel-heading"><h2>重置识别与联动</h2><UiIcon name="shield" :size="19" /></div><ol class="flow-steps"><li><span class="step-number mono">01</span><div><strong>读取主站已有快照</strong><p>首个有效快照只建立基线；每 {{ config.poll_interval_seconds }} 秒检查主站账号记录。</p></div></li><li><span class="step-number mono">02</span><div><strong>识别并复核重置</strong><p>7d 快照用量从非零归零，再次读取主站记录复核；不主动获取或补刷 OpenAI 额度。</p></div></li><li><span class="step-number mono">03</span><div><strong>通知并按规则执行</strong><p>快照缺失、未更新或时间异常时等待 / 标记未知，不回退查询上游。有效事件去重后按规则执行。</p></div></li></ol><div class="mechanism-summary"><span>全局自动重置</span><span class="status-pill" :class="{ success: config.auto_reset_enabled }">{{ config.auto_reset_enabled ? '已开启' : '关闭' }}</span></div><button class="text-button" type="button" @click="view = 'settings'">管理检测与通知设置 <UiIcon name="arrow" :size="15" /></button></section>
            </div>
            <section class="panel recent-panel"><div class="panel-heading"><h2>最近重置</h2><button class="text-button" type="button" @click="view = 'history'">查看全部 <UiIcon name="arrow" :size="16" /></button></div><div v-if="latestEvents.length" class="recent-events"><article v-for="event in latestEvents" :key="event.id"><span class="event-icon"><UiIcon name="refresh" :size="18" /></span><div><strong>{{ event.account_name }}</strong><p>{{ event.rule_names?.join('、') || '未关联规则' }}</p></div><div class="recent-change mono">{{ percent(event.previous_percent) }} <span>→</span> <span class="accent">{{ percent(event.used_percent) }}</span><small>主站采样 {{ dateTime(event.sampled_at) }}<br />本服务发现 {{ dateTime(event.detected_at) }}</small></div></article></div><div v-else class="subtle-empty">检测到账号额度重置后，这里会展示变化与命中规则。</div></section>
          </div>
          <div v-if="view === 'accounts'">
            <section class="panel"><div class="panel-heading"><div><h2>选择来源账号</h2><p class="muted">支持普通 Codex 与 Spark 的 OpenAI OAuth 账号</p></div><button class="button primary" type="button" :disabled="!selectedAccounts.length || !connectionReady" @click="newRule(selectedAccounts)"><UiIcon name="plus" :size="17" /> 用所选 {{ selectedAccounts.length }} 个账号创建规则</button></div><AccountPicker v-if="connectionReady" v-model="selectedAccounts" :watched-ids="watchedIds" @loaded="rememberAccounts" /><div v-else class="empty-state"><h3>源站尚未连接</h3><p>保存地址与管理员 API Key 后读取账号列表。</p></div></section><p class="page-note">勾选仅保存在当前页面草稿中。保存启用规则后检查主站已有快照；规则中的任一账号重置均可触发通知与联动。Spark 需等待主站已有快照更新，本服务不主动刷新上游。</p>
          </div>
          <div v-show="view === 'rules'" class="rules-layout">
            <RuleEditor v-if="editor" :key="editor.id" :rule="editor" :is-new="editorIsNew" :global-auto-reset="config.auto_reset_enabled" :saving="pending.has('rules')" :simulating="pending.has('simulate')" :simulation="simulation" @save="saveRule" @simulate="simulateRule" @cancel="editor = null; simulation = null" />
            <section class="panel"><div class="panel-heading"><div><h2>已保存规则</h2><p class="muted">{{ enabledRules.length }} 条启用 · {{ state.rules.length - enabledRules.length }} 条停用</p></div><span class="status-pill">任一来源账号触发</span></div><div v-if="state.rules.length" class="rule-list"><article v-for="rule in state.rules" :key="rule.id" class="rule-card" :class="{ disabled: !rule.enabled }"><div class="rule-card-top"><div><h3>{{ rule.name }}</h3><span class="status-pill" :class="{ success: rule.enabled }">{{ rule.enabled ? '监控中' : '已停用' }}</span></div><input class="switch" type="checkbox" role="switch" :checked="rule.enabled" :disabled="pending.has('rules')" :aria-label="`启用规则 ${rule.name}`" @change="toggleRule(rule)" /></div><div class="rule-card-details"><span><UiIcon name="users" :size="15" /> {{ rule.account_ids.length }} 个账号</span><span><UiIcon name="link" :size="15" /> {{ rule.subscription_ids.length }} 条订阅</span><span><UiIcon name="bell" :size="15" /> {{ [rule.notify_telegram && 'Telegram', rule.notify_email && '邮件'].filter(Boolean).join(' + ') || '不发送通知' }}</span></div><div class="rule-card-footer"><span class="caption muted">{{ config.auto_reset_enabled && rule.auto_reset ? `自动重置${maskText(rule)}` : !config.auto_reset_enabled && config.telegram.enabled && rule.notify_telegram && rule.subscription_ids.length && (rule.daily || rule.weekly || rule.monthly) ? `Telegram 手动确认 · ${maskText(rule)}` : '仅检测与通知' }}</span><div><button class="button small secondary" type="button" :disabled="pending.has('rules')" @click="editRule(rule)"><UiIcon name="edit" :size="14" /> 编辑</button><button class="button small ghost" type="button" :disabled="pending.has('rules')" @click="deleteRule(rule)">删除</button></div></div></article></div><div v-else class="empty-state"><UiIcon name="rules" :size="30" /><h3>用一条规则连接账号与订阅</h3><p>选择来源账号、通知渠道和目标订阅。自动重置默认关闭。</p><button class="button primary" type="button" :disabled="!connectionReady" @click="newRule()"><UiIcon name="plus" :size="18" /> 创建第一条规则</button></div></section>
          </div>
          <EventHistory v-if="view === 'history'" :state="state" :retrying="retrying" @retry="retryAction" />
          <SettingsPanel v-if="ready" v-show="view === 'settings'" :config="config" :receiver="state.telegram_receiver" :update="state.update" :acknowledging-update="pending.has('update-acknowledge')" :acknowledge-error="updateAcknowledgeError" :saving="pending.has('config')" :testing-connection="pending.has('connection')" :testing-telegram="pending.has('telegram')" :testing-email="pending.has('email')" @save="saveConfig" @test-connection="testConnection" @test-notification="testNotification" @acknowledge-update="acknowledgeUpdate" />
        </template>
        <footer class="workspace-footer"><span>Quota Watch</span><span>账号额度变化 · 通知 · 订阅联动</span></footer>
      </main>
    </div>
  </div>
</template>
