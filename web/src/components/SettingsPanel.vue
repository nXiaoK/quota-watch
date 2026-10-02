<script setup lang="ts">
import { computed, ref, toRaw, watch } from 'vue'
import { dateTime, statusText } from '../format'
import type { Config, TelegramReceiverState, UpdateApproval, UpdateState } from '../types'
import UiIcon from './UiIcon.vue'

const props = defineProps<{ config: Config; receiver: TelegramReceiverState; update: UpdateState; approval?: UpdateApproval; acknowledgingUpdate: boolean; acknowledgeError: string; saving: boolean; testingConnection: boolean; testingTelegram: boolean; testingEmail: boolean }>()
const emit = defineEmits<{ save: [config: Config]; testConnection: [config: Config]; testNotification: [channel: 'telegram' | 'email', config: Config]; acknowledgeUpdate: [] }>()
const draft = ref<Config>(structuredClone(toRaw(props.config)))
const recipients = ref(props.config.email.recipients.join('\n'))
const allowedUsers = ref((props.config.telegram.allowed_user_ids ?? []).join('\n'))
const validation = ref('')
const sourceChanged = computed(() => draft.value.base_url.trim().replace(/\/+$/, '') !== props.config.base_url.trim().replace(/\/+$/, ''))
const telegramGroup = computed(() => /^(?:-|@)/.test(draft.value.telegram.chat_id.trim()))
const updateStatus = computed(() => ({
  disabled: '已关闭', up_to_date: '已是最新版本', available: '发现新版本', waiting_idle: '等待空闲',
  checking: '正在检查更新', check_failed: '检查更新失败', updating: '正在更新', restarting: '等待站点重启',
  success: '更新成功', failed: '更新失败', unknown: '结果待核对', already_attempted: '已尝试此版本',
  awaiting_approval: '等待 Telegram 确认', queued: '已加入更新队列', declined: '已拒绝此版本',
  approval_unavailable: 'Telegram 确认不可用', waiting_engine: '等待其他任务结束',
} as Record<string, string>)[props.update.status] ?? (props.update.status || '尚无记录'))
const updateTrigger = computed(() => props.update.last_trigger === 'idle' ? '空闲触发' : props.update.last_trigger === 'scheduled' ? '定时触发' : props.update.last_trigger || '')
const updateApprovalStatus = computed(() => ({
  pending: '等待 Telegram 确认', approved: '已批准，等待自动更新条件',
  declined: '已拒绝此版本', superseded: '已有新版本，此选择已失效',
  expired: '确认已过期', invalid: '配置或版本已变化',
} as Record<string, string>)[props.approval?.status ?? ''] ?? (props.approval?.status || '—'))

watch(() => props.config, (config) => {
  draft.value = structuredClone(toRaw(config))
  recipients.value = config.email.recipients.join('\n')
  allowedUsers.value = (config.telegram.allowed_user_ids ?? []).join('\n')
  validation.value = ''
})

function payload(): Config | null {
  validation.value = ''
  if (!draft.value.base_url.trim()) validation.value = '请填写 Sub2API 地址。'
  else if (!Number.isInteger(draft.value.poll_interval_seconds) || draft.value.poll_interval_seconds < 10) validation.value = '检测间隔必须是至少 10 秒的整数。'
  else if (!Number.isInteger(draft.value.confirm_delay_seconds) || draft.value.confirm_delay_seconds < 1 || draft.value.confirm_delay_seconds > 60) validation.value = '复核间隔必须是 1 至 60 秒的整数。'
  else if (!Number.isInteger(draft.value.concurrency) || draft.value.concurrency < 1 || draft.value.concurrency > 20) validation.value = '并行读取数必须是 1 至 20 的整数。'
  const update = draft.value.update
  const timePattern = /^(?:[01]\d|2[0-3]):[0-5]\d$/
  if (!validation.value && (!timePattern.test(update.window_start) || !timePattern.test(update.window_end))) validation.value = '自动更新时段请填写有效的 24 小时时间。'
  else if (!validation.value && update.window_start >= update.window_end) validation.value = '自动更新时段的结束时间必须晚于开始时间，且应在同一天。'
  update.timezone = update.timezone.trim()
  if (!validation.value) {
    try { new Intl.DateTimeFormat('en-US', { timeZone: update.timezone }).format(0) }
    catch { validation.value = '请输入有效的 IANA 时区，例如 Asia/Shanghai。' }
  }
  const userIDs = allowedUsers.value.split(/[\s,;，；]+/).filter(Boolean)
  if (!validation.value && userIDs.some((value) => !/^\d+$/.test(value) || !Number.isSafeInteger(Number(value)) || Number(value) <= 0)) validation.value = '允许操作的 Telegram 用户 ID 必须是正整数，每行一个或使用逗号分隔。'
  if (validation.value) return null
  const config = structuredClone(toRaw(draft.value))
  config.base_url = config.base_url.trim()
  config.telegram.allowed_user_ids = [...new Set(userIDs.map(Number))]
  config.email.recipients = [...new Set(recipients.value.split(/[\n,;，；]+/).map((value) => value.trim()).filter(Boolean))]
  return config
}

function submit(action: 'save' | 'connection' | 'telegram' | 'email') {
  const config = payload()
  if (!config) return
  if (action === 'save') emit('save', config)
  else if (action === 'connection') emit('testConnection', config)
  else emit('testNotification', action, config)
}
</script>

<template>
  <div class="settings-layout">
    <section class="panel settings-section">
      <div class="panel-heading"><div class="section-title"><span class="section-icon"><UiIcon name="link" /></span><div><h2>Sub2API 连接</h2><p class="muted">连接管理员 API，独立保存监控配置</p></div></div><span class="status-pill">源站</span></div>
      <div class="settings-body">
        <label class="field"><span>Sub2API 地址</span><input v-model="draft.base_url" type="url" placeholder="https://sub2api.example.com" autocomplete="url" /><small>填写站点根地址。跨机器连接建议使用 HTTPS。</small></label>
        <label class="field"><span>管理员 API Key <span v-if="config.admin_api_key_configured" class="status-pill success">已配置</span></span><input v-model="draft.admin_api_key" type="password" autocomplete="new-password" spellcheck="false" :placeholder="config.admin_api_key_configured ? '留空保留已保存的密钥' : '填入管理员 API Key'" :disabled="draft.clear_admin_api_key" /><small>使用主站已有的管理员 Key；重生成主站 Key 会使旧连接失效。</small></label>
        <label v-if="config.admin_api_key_configured" class="checkbox-label"><input v-model="draft.clear_admin_api_key" type="checkbox" /> 清除已保存的管理员 API Key</label>
        <div class="inline-message muted-note">管理员 Key 具有管理权限，凭据仅由服务端保存。输入留空时保留原值，保存后页面会清空输入框。</div>
        <div v-if="sourceChanged && config.base_url" class="inline-message warning">切换源站会停用已有规则和自动更新、清除检测基线并跳过未执行的旧动作。请重新核对站点、账号与订阅 ID 后重新启用。</div>
        <button class="button secondary" type="button" :disabled="testingConnection || saving" @click="submit('connection')"><UiIcon name="link" :size="17" /> {{ testingConnection ? '正在连接…' : '测试管理员 API 连接' }}</button>
      </div>
    </section>
    <section class="panel settings-section">
      <div class="panel-heading"><div class="section-title"><span class="section-icon"><UiIcon name="pulse" /></span><div><h2>快照检查与自动执行</h2><p class="muted">规则与全局开关共同控制执行</p></div></div></div>
      <div class="settings-body">
        <div class="form-grid three"><label class="field"><span>快照检查间隔（秒）</span><input v-model.number="draft.poll_interval_seconds" type="number" min="10" step="1" /></label><label class="field"><span>再次读取间隔（秒）</span><input v-model.number="draft.confirm_delay_seconds" type="number" min="1" max="60" step="1" /></label><label class="field"><span>并行读取数</span><input v-model.number="draft.concurrency" type="number" min="1" max="20" step="1" /></label></div>
        <p class="muted caption">默认每 60 秒检查 Sub2API 账号记录中的已有额度快照。并行读取数控制主站读取并发，不限制账号总数；复核仅再次读取主站已保存记录。</p><p class="muted caption">不主动获取 OpenAI 额度，不补刷快照，也不通过上游接口兜底。快照缺失、未更新或时间字段异常时显示等待 / 未知，不按 0% 处理。</p>
        <label class="toggle-row"><span><strong>全局自动重置</strong><small>开启后，仅执行同样开启“自动重置”的规则；关闭时继续检测，并可通过 Telegram 手动确认重置</small></span><input v-model="draft.auto_reset_enabled" class="switch" type="checkbox" role="switch" aria-label="全局自动重置" /></label>
        <p class="muted caption">全局自动重置关闭时，启用 Telegram 并在规则中选择 Telegram、目标订阅和额度周期。检测到归零后，通知内可选择重置或忽略，无需开启规则的自动重置。</p>
        <p class="muted caption">主站快照变化会复核并持久化去重。订阅重置只清零选中周期的用量，不调整订阅到期时间。</p>
        <label class="toggle-row"><span><strong>详细运行日志</strong><small>记录快照检查、自动更新的检查/空闲/重启核对阶段，以及 Sub2API 接口请求的状态与耗时。默认关闭，保存后立即生效。</small></span><input v-model="draft.verbose_logging" class="switch" type="checkbox" role="switch" aria-label="详细运行日志" /></label>
        <p class="muted caption">在 Docker 中使用 <code class="mono">docker compose logs -f quota-watch</code> 实时查看。关闭后不输出接口明细，更新开始、成功及异常仍会记录；日志不包含管理员 Key、Cookie 或请求与响应正文。</p>
      </div>
    </section>
    <section class="panel settings-section">
      <div class="panel-heading"><div class="section-title"><span class="section-icon"><UiIcon name="refresh" /></span><div><h2>Sub2API 自动更新</h2><p class="muted">发现新版本后通知确认，按空闲或指定时段更新站点</p></div></div></div>
      <div class="settings-body">
        <label class="toggle-row"><span><strong>空闲时自动更新</strong><small>需要连续 10 分钟没有任何使用记录；记录读取失败时不会判断为空闲。</small></span><input v-model="draft.update.idle_enabled" class="switch" type="checkbox" role="switch" aria-label="空闲时自动更新 Sub2API" /></label>
        <label class="toggle-row"><span><strong>指定时段自动更新</strong><small>每天在下方时段内检查新版本，并等待满足连续 10 分钟无使用记录；窗口结束仍不空闲就跳过当天。</small></span><input v-model="draft.update.scheduled_enabled" class="switch" type="checkbox" role="switch" aria-label="指定时段自动更新 Sub2API" /></label>
        <div class="form-grid three update-window"><label class="field"><span>开始时间</span><input v-model="draft.update.window_start" type="time" step="60" /></label><label class="field"><span>结束时间</span><input v-model="draft.update.window_end" type="time" step="60" /></label><label class="field"><span>时区</span><input v-model="draft.update.timezone" type="text" placeholder="Asia/Shanghai" autocomplete="off" /><small>使用 IANA 时区名称。</small></label></div>
        <p class="muted caption">两个开关可以独立使用；指定时段默认是 Asia/Shanghai 每天 02:00–03:00。自动更新与上方的订阅自动重置分别控制。</p>
        <label class="toggle-row"><span><strong>发现新版本时 Telegram 确认</strong><small>检测到新版本后发送带 GitHub 发布页链接的通知；在 Telegram 中选择加入空闲更新队列或拒绝此版本。需启用并配置下方的 Telegram 通道。</small></span><input v-model="draft.update.notify_available_telegram_enabled" class="switch" type="checkbox" role="switch" aria-label="发现 Sub2API 新版本时通过 Telegram 确认" /></label>
        <p class="muted caption">开启后，只有确认的版本才会按上面的空闲或时段规则尝试安装；安装前会再次检查最新版本。拒绝某版本后，即使关闭此开关也不会自动安装该版本；关闭开关后，其他版本恢复无需确认的自动更新。两个自动更新开关都关闭时仍会检查并通知，但不会安装。</p>
        <label class="toggle-row"><span><strong>时段结束未更新时通知 Telegram</strong><small>指定时段结束后，若有待更新版本或检查失败，发送一次未更新原因及最后检查时间；没有新版本或已拒绝的版本不提醒。需开启指定时段自动更新及下方 Telegram 通道。</small></span><input v-model="draft.update.notify_window_missed_telegram_enabled" class="switch" type="checkbox" role="switch" aria-label="Sub2API 时段结束未更新时通知 Telegram" /></label>
        <label class="toggle-row"><span><strong>更新成功后通知 Telegram</strong><small>更新、重启及版本核对成功后发送；还需启用并配置下方的 Telegram 通道。</small></span><input v-model="draft.update.notify_telegram_enabled" class="switch" type="checkbox" role="switch" aria-label="Sub2API 更新成功后通知 Telegram" /></label>
        <label class="toggle-row"><span><strong>更新成功后通知邮件</strong><small>更新、重启及版本核对成功后发送；还需启用并配置下方的邮件通道。</small></span><input v-model="draft.update.notify_email_enabled" class="switch" type="checkbox" role="switch" aria-label="Sub2API 更新成功后通知邮件" /></label>
        <div class="inline-message warning">Sub2API 若运行在 Docker 容器中，API 原地更新不会写回镜像，重建容器后更新会丢失；请通过更新镜像部署新版本。</div>
        <div class="action-record">
          <div class="action-record-heading"><div><strong>最近更新状态</strong><span class="status-pill" :class="{ success: ['up_to_date', 'success'].includes(update.status), error: ['check_failed', 'failed', 'unknown'].includes(update.status) }">{{ updateStatus }}</span></div></div>
          <div class="update-status-grid">
            <p class="caption muted">当前版本：{{ update.current_version || '—' }}</p>
            <p class="caption muted">最新版本：{{ update.latest_version || '—' }}</p>
            <p class="caption muted">最近检查：{{ dateTime(update.last_check_at) }}</p>
            <p class="caption muted">最近尝试：{{ dateTime(update.last_attempt_at) }}</p>
            <p class="caption muted">最近成功：{{ dateTime(update.last_success_at) }}</p>
            <p class="caption muted">触发方式：{{ updateTrigger || '—' }}</p>
            <p v-if="approval?.version" class="caption muted">版本确认：{{ updateApprovalStatus }}（{{ approval.version }}）</p>
          </div>
          <p v-if="update.last_error" class="error-text">{{ update.last_error }}</p>
          <div v-if="update.status === 'unknown'" class="update-recovery">
            <p class="caption muted">请先在 Sub2API 核对当前版本；必要时手动重启并确认站点恢复，再允许自动更新重新尝试。</p>
            <button class="button small secondary" type="button" :disabled="acknowledgingUpdate" @click="emit('acknowledgeUpdate')"><UiIcon name="check" :size="15" /> {{ acknowledgingUpdate ? '正在确认…' : '已核对，允许再次自动尝试' }}</button>
            <div v-if="acknowledgeError" class="inline-message error" role="alert">{{ acknowledgeError }}</div>
          </div>
        </div>
      </div>
    </section>
    <section class="panel settings-section">
      <div class="panel-heading"><div class="section-title"><span class="section-icon"><UiIcon name="bell" /></span><div><h2>Telegram</h2><p class="muted">向个人聊天或群组发送重置及更新通知</p></div></div><input v-model="draft.telegram.enabled" class="switch" type="checkbox" role="switch" aria-label="启用 Telegram 通知渠道" /></div>
      <div class="settings-body">
        <label class="field"><span>Bot Token <span v-if="config.telegram.bot_token_configured" class="status-pill success">已配置</span></span><input v-model="draft.telegram.bot_token" type="password" autocomplete="new-password" spellcheck="false" :placeholder="config.telegram.bot_token_configured ? '留空保留已保存的 Token' : '填入 Bot Token'" :disabled="draft.clear_telegram_token" /></label>
        <label v-if="config.telegram.bot_token_configured" class="checkbox-label"><input v-model="draft.clear_telegram_token" type="checkbox" /> 清除已保存的 Bot Token</label>
        <label class="field"><span>Chat ID</span><input v-model="draft.telegram.chat_id" type="text" placeholder="例如：123456789 或 -1001234567890" autocomplete="off" /><small>需先向机器人发送消息，或将机器人加入目标群组。</small></label>
        <label v-if="telegramGroup" class="field"><span>允许操作的 Telegram 用户 ID（可选）</span><textarea v-model="allowedUsers" rows="3" inputmode="numeric" placeholder="每行一个 Telegram 用户 ID，或使用逗号分隔" /><small>填写用户的数字 ID，不是 @用户名。群组需指定允许操作重置或更新按钮的用户。</small></label>
        <div v-if="telegramGroup && !allowedUsers.trim()" class="inline-message muted-note">当前群组未指定允许操作的用户。通知仍会发送，群成员不能确认重置或更新。</div>
        <p v-if="draft.telegram.chat_id.trim() && !telegramGroup" class="muted caption">个人私聊仅收件人本人可以操作重置或更新按钮，无需填写授权用户。</p>
        <label class="field"><span>代理地址（可选）<span v-if="config.telegram.proxy_url_configured" class="status-pill success">已配置</span></span><input v-model="draft.telegram.proxy_url" type="password" autocomplete="new-password" spellcheck="false" :placeholder="config.telegram.proxy_url_configured ? '留空保留已保存的代理' : 'http://… 或 socks5://…'" :disabled="draft.clear_telegram_proxy" /><small>代理地址可能包含密码，按敏感信息保存，不在页面回显。</small></label>
        <label v-if="config.telegram.proxy_url_configured" class="checkbox-label"><input v-model="draft.clear_telegram_proxy" type="checkbox" /> 清除已保存的代理地址</label>
        <p class="muted caption">按钮确认无需配置公网地址。请使用专供本服务的 Bot，避免其他程序或已有 Webhook 占用按钮接收。</p>
        <div class="action-record">
          <div class="action-record-heading"><div><strong>按钮接收状态</strong><span class="status-pill" :class="{ success: ['healthy', 'ok', 'listening', 'polling', 'running'].includes(receiver.status), error: ['error', 'conflict', 'webhook'].includes(receiver.status) }">{{ receiver.status ? statusText(receiver.status) : '尚未启动' }}</span></div></div>
          <p class="caption muted">最近检查 {{ dateTime(receiver.checked_at) }}</p>
          <p v-if="receiver.last_error" class="error-text">{{ receiver.last_error }}</p>
          <p v-if="['error', 'conflict', 'webhook'].includes(receiver.status)" class="muted caption">若提示接收冲突或 Webhook 占用，请停止其他程序使用此 Bot，或改用独立 Bot 后保存设置。</p>
        </div>
        <button class="button secondary" type="button" :disabled="testingTelegram || saving" @click="submit('telegram')"><UiIcon name="bell" :size="17" /> {{ testingTelegram ? '正在发送…' : '发送 Telegram 测试通知' }}</button>
      </div>
    </section>
    <section class="panel settings-section">
      <div class="panel-heading"><div class="section-title"><span class="section-icon"><UiIcon name="bell" /></span><div><h2>邮件</h2><p class="muted">配置本服务独立使用的 SMTP 通道</p></div></div><input v-model="draft.email.enabled" class="switch" type="checkbox" role="switch" aria-label="启用邮件通知渠道" /></div>
      <div class="settings-body">
        <div class="form-grid"><label class="field"><span>SMTP 主机</span><input v-model="draft.email.host" type="text" placeholder="smtp.example.com" autocomplete="off" /></label><label class="field"><span>SMTP 端口</span><input v-model.number="draft.email.port" type="number" min="1" max="65535" /></label></div>
        <label class="field"><span>加密连接</span><select v-model="draft.email.tls_mode"><option value="starttls">STARTTLS（通常为 587 端口）</option><option value="tls">TLS（通常为 465 端口）</option></select></label>
        <div class="form-grid"><label class="field"><span>SMTP 用户名</span><input v-model="draft.email.username" type="text" autocomplete="off" /></label><label class="field"><span>密码 / 授权码 <span v-if="config.email.password_configured" class="status-pill success">已配置</span></span><input v-model="draft.email.password" type="password" autocomplete="new-password" :placeholder="config.email.password_configured ? '留空保留已保存的密码' : '填入密码或授权码'" :disabled="draft.clear_smtp_password" /></label></div>
        <label v-if="config.email.password_configured" class="checkbox-label"><input v-model="draft.clear_smtp_password" type="checkbox" /> 清除已保存的 SMTP 密码</label>
        <label class="field"><span>发件地址</span><input v-model="draft.email.from" type="email" placeholder="notify@example.com" /></label>
        <label class="field"><span>收件地址</span><textarea v-model="recipients" rows="3" placeholder="每行一个地址，或使用逗号分隔" /><small>支持多个收件人。不会读取主站 SMTP 密码。</small></label>
        <button class="button secondary" type="button" :disabled="testingEmail || saving" @click="submit('email')"><UiIcon name="bell" :size="17" /> {{ testingEmail ? '正在发送…' : '发送邮件测试通知' }}</button>
      </div>
    </section>
    <div class="settings-save"><span class="muted">测试使用当前表单配置，不会自动保存；通知测试会真实发送消息。</span><button class="button primary" type="button" :disabled="saving || testingConnection || testingTelegram || testingEmail" @click="submit('save')"><UiIcon name="check" :size="18" /> {{ saving ? '正在保存…' : '保存全部设置' }}</button></div>
    <div v-if="validation" class="inline-message error" role="alert">{{ validation }}</div>
  </div>
</template>
