<script setup lang="ts">
import { computed, ref } from 'vue'
import { dateTime, maskText, percent, statusText } from '../format'
import type { Action, ManualRequest, State } from '../types'
import UiIcon from './UiIcon.vue'

const props = defineProps<{ state: State; retrying: string[] }>()
const emit = defineEmits<{ retry: [action: Action] }>()
const tab = ref<'events' | 'actions' | 'manual' | 'deliveries'>('events')
const search = ref('')
const status = ref('')
const query = computed(() => search.value.trim().toLowerCase())
const eventsById = computed(() => new Map(props.state.events.map((event) => [event.id, event])))
const actionsById = computed(() => new Map(props.state.actions.map((action) => [action.id, action])))
const events = computed(() => [...props.state.events].filter((event) => !query.value || `${event.account_name} ${event.account_id} ${event.rule_names?.join(' ')}`.toLowerCase().includes(query.value)).sort((a, b) => Date.parse(b.detected_at) - Date.parse(a.detected_at)))
const actions = computed(() => [...props.state.actions].filter((action) => (!status.value || action.status === status.value) && (!query.value || `${action.id} ${action.manual_request_id ?? ''} ${action.subscription_ids.join(' ')} ${actionSources(action).join(' ')} ${action.last_error ?? ''}`.toLowerCase().includes(query.value))).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)))
const manualRequests = computed(() => [...props.state.manual_requests].filter((request) => (!status.value || request.status === status.value) && (!query.value || `${request.id} ${request.targets.map((target) => target.subscription_id).join(' ')} ${manualSources(request).join(' ')} ${request.approved_by ?? ''} ${request.last_error ?? ''}`.toLowerCase().includes(query.value))).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)))
const deliveries = computed(() => [...props.state.deliveries].filter((delivery) => (!status.value || delivery.status === status.value) && (!query.value || `${delivery.channel} ${delivery.manual_request_id ?? ''} ${delivery.message} ${delivery.last_error ?? ''}`.toLowerCase().includes(query.value))).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)))
const historyPage = ref(1)
const visibleEvents = computed(() => events.value.slice((historyPage.value - 1) * 20, historyPage.value * 20))
const visibleActions = computed(() => actions.value.slice((historyPage.value - 1) * 20, historyPage.value * 20))
const visibleDeliveries = computed(() => deliveries.value.slice((historyPage.value - 1) * 20, historyPage.value * 20))
const visibleManualRequests = computed(() => manualRequests.value.slice((historyPage.value - 1) * 20, historyPage.value * 20))
const count = computed(() => tab.value === 'events' ? events.value.length : tab.value === 'actions' ? actions.value.length : tab.value === 'manual' ? manualRequests.value.length : deliveries.value.length)
const pageCount = computed(() => Math.max(1, Math.ceil(count.value / 20)))
const statuses = computed(() => tab.value === 'manual' ? ['pending', 'processing', 'succeeded', 'ignored', 'expired', 'invalid', 'invalidated', 'superseded', 'failed', 'partial', 'unknown', 'skipped'] : tab.value === 'actions' ? ['pending', 'running', 'succeeded', 'failed', 'partial', 'unknown', 'retried', 'skipped'] : ['pending', 'running', 'succeeded', 'failed', 'skipped'])

function actionEventIds(action: Action): string[] {
  return [...new Set([...(action.event_ids ?? []), action.event_id].filter(Boolean))]
}

function actionSources(action: Action): string[] {
  return actionEventIds(action).map((id) => {
    const event = eventsById.value.get(id)
    return event ? `${event.account_name || '账号'} #${event.account_id}` : `历史事件 ${id}`
  })
}

function manualSources(request: ManualRequest): string[] {
  return request.event_ids.map((id) => {
    const event = eventsById.value.get(id)
    return event ? `${event.account_name || '账号'} #${event.account_id}` : `历史事件 ${id}`
  })
}

function selectTab(value: typeof tab.value) {
  tab.value = value
  status.value = ''
  historyPage.value = 1
}
</script>

<template>
  <section class="panel history-panel">
    <div class="editor-tabs" role="tablist" aria-label="历史记录类型">
      <button id="history-tab-events" type="button" role="tab" :aria-selected="tab === 'events'" aria-controls="history-results" :class="{ active: tab === 'events' }" @click="selectTab('events')">重置事件 <span class="mono">{{ state.events.length }}</span></button>
      <button id="history-tab-manual" type="button" role="tab" :aria-selected="tab === 'manual'" aria-controls="history-results" :class="{ active: tab === 'manual' }" @click="selectTab('manual')">手动确认 <span class="mono">{{ state.manual_requests.length }}</span></button>
      <button id="history-tab-actions" type="button" role="tab" :aria-selected="tab === 'actions'" aria-controls="history-results" :class="{ active: tab === 'actions' }" @click="selectTab('actions')">订阅执行 <span class="mono">{{ state.actions.length }}</span></button>
      <button id="history-tab-deliveries" type="button" role="tab" :aria-selected="tab === 'deliveries'" aria-controls="history-results" :class="{ active: tab === 'deliveries' }" @click="selectTab('deliveries')">通知记录 <span class="mono">{{ state.deliveries.length }}</span></button>
    </div>
    <div class="filter-bar history-filters"><label class="search-field"><span class="sr-only">搜索历史记录</span><UiIcon name="search" :size="18" /><input v-model="search" type="search" :placeholder="tab === 'events' ? '搜索账号或规则名称' : tab === 'actions' ? '搜索来源账号、订阅 ID、动作 ID 或错误' : tab === 'manual' ? '搜索来源账号、订阅 ID、请求 ID 或操作用户' : '搜索通知内容或渠道'" @input="historyPage = 1" /></label><label v-if="tab !== 'events'" class="filter-field"><span class="sr-only">筛选记录状态</span><select v-model="status" @change="historyPage = 1"><option value="">全部状态</option><option v-for="value in statuses" :key="value" :value="value">{{ tab === 'manual' && value === 'pending' ? '等待 Telegram 确认' : statusText(value) }}</option></select></label></div>
    <div id="history-results" role="tabpanel" :aria-labelledby="`history-tab-${tab}`">
      <div v-if="tab === 'events'" class="table-scroll"><table><thead><tr><th>账号 / 重置维度</th><th>使用率变化</th><th>命中规则</th><th>主站采样 / 发现时间</th></tr></thead><tbody><tr v-for="event in visibleEvents" :key="event.id"><td><div class="table-primary">{{ event.account_name || `账号 #${event.account_id}` }}</div><span class="caption muted mono">#{{ event.account_id }} · {{ event.dimension === 'spark' ? 'Spark' : '普通 Codex' }} 7d</span><div class="caption muted">来源：{{ event.source === 'stored' ? '主站快照（stored）' : '未知' }}</div></td><td><div class="percent-transition"><span class="mono muted">{{ percent(event.previous_percent) }}</span><UiIcon name="arrow" :size="16" /><span class="mono accent">{{ percent(event.used_percent) }}</span></div><span class="caption muted">快照记录的重置时间 {{ dateTime(event.reset_at) }}</span></td><td>{{ event.rule_names?.join('、') || '—' }}</td><td class="mono caption"><div>主站采样 {{ dateTime(event.sampled_at) }}</div><div class="muted">本服务发现 {{ dateTime(event.detected_at) }}</div></td></tr><tr v-if="!visibleEvents.length"><td colspan="4" class="table-empty"><UiIcon name="history" :size="28" /><strong>暂无重置事件</strong><span>有过有效非零基线，并通过 7d 重置复核后，事件会显示在这里。</span></td></tr></tbody></table></div>
      <div v-else-if="tab === 'manual'" class="action-list">
        <article v-for="request in visibleManualRequests" :key="request.id" class="action-record">
          <div class="action-record-heading"><div><span class="status-pill" :class="{ success: ['completed', 'succeeded'].includes(request.status), error: ['failed', 'partial', 'unknown'].includes(request.status) }">{{ request.status === 'pending' ? '等待 Telegram 确认' : request.status === 'succeeded' ? '已完成' : statusText(request.status) }}</span><strong>{{ request.targets.length }} 条订阅 · Telegram 手动重置</strong></div><span class="caption muted">{{ dateTime(request.created_at) }}</span></div>
          <p class="caption muted">关联来源：{{ manualSources(request).join('、') || '—' }} · Chat ID {{ request.chat_id }}</p>
          <p v-if="request.status === 'pending'" class="muted caption">请在 Telegram 通知内选择“重置选定订阅”或“忽略”。确认有效期至 {{ dateTime(request.expires_at) }}。</p>
          <p v-if="request.approved_by" class="caption muted">批准用户 #{{ request.approved_by }} · {{ dateTime(request.decision_at) }}</p>
          <p v-else-if="dateTime(request.decision_at) !== '—'" class="caption muted">处理时间 {{ dateTime(request.decision_at) }}</p>
          <p v-if="request.last_error" class="error-text">{{ request.last_error }}</p>
          <p v-if="request.status === 'unknown'" class="muted caption">请求结果未知。请先在主站核对用量，服务不会再次执行确认。</p>
          <details><summary>查看重置目标与关联执行结果</summary><div class="action-details">
            <p class="caption mono muted">请求 {{ request.id }}</p>
            <ul class="detail-list"><li v-for="target in request.targets" :key="target.subscription_id"><span>订阅 #{{ target.subscription_id }} · {{ maskText(target.mask) }}</span><span class="caption muted">用户 #{{ target.ref.user_id }} · 分组 #{{ target.ref.group_id }}</span></li></ul>
            <p v-if="!request.action_ids.length" class="muted caption">尚无订阅执行动作。</p>
            <div v-for="id in request.action_ids" :key="id">
              <p class="caption mono muted">动作 {{ id }} · {{ statusText(actionsById.get(id)?.status || 'unknown') }}</p>
              <ul class="detail-list"><li v-for="item in actionsById.get(id)?.results" :key="item.subscription_id"><span class="mono">订阅 #{{ item.subscription_id }}</span><span :class="item.success ? 'accent' : 'error-text'">{{ item.success ? '成功' : item.error || '失败' }}</span></li></ul>
            </div>
          </div></details>
        </article>
        <div v-if="!visibleManualRequests.length" class="empty-state compact"><UiIcon name="bell" :size="28" /><h3>暂无手动确认记录</h3><p>全局自动重置关闭时，为规则选择 Telegram、目标订阅和额度周期；检测到归零后可在 Telegram 通知内重置或忽略。</p></div>
      </div>
      <div v-else-if="tab === 'actions'" class="action-list">
        <article v-for="action in visibleActions" :key="action.id" class="action-record">
          <div class="action-record-heading"><div><span class="status-pill" :class="{ success: action.status === 'succeeded', error: ['failed', 'partial', 'unknown'].includes(action.status) }">{{ statusText(action.status) }}</span><strong>{{ action.subscription_ids.length }} 条订阅 · {{ maskText(action.mask) }}</strong><span class="status-pill">{{ action.mode === 'manual' ? 'Telegram 手动' : '自动重置' }}</span></div><button v-if="['failed', 'partial'].includes(action.status)" class="button small secondary" type="button" :disabled="retrying.includes(action.id)" @click="emit('retry', action)"><UiIcon name="refresh" :size="15" /> {{ retrying.includes(action.id) ? '正在重试…' : '仅重试失败项' }}</button></div>
          <p class="caption muted">关联来源：{{ actionSources(action).join('、') || '—' }}</p><p v-if="action.manual_request_id" class="caption mono muted">手动确认请求 {{ action.manual_request_id }}</p><p v-if="action.last_error" class="error-text">{{ action.last_error }}</p><p v-if="action.status === 'unknown'" class="muted caption">请求结果未知。请先在主站核对用量，服务不会直接重试，以免再次清零。</p><p class="caption muted">{{ dateTime(action.created_at) }} · 尝试 {{ action.attempts }} 次</p>
          <details><summary>查看各订阅结果与事件编号</summary><div class="action-details"><p class="caption mono muted">动作 {{ action.id }}</p><ul class="detail-list"><li v-for="id in actionEventIds(action)" :key="id"><span>{{ eventsById.get(id)?.account_name || (eventsById.get(id) ? `账号 #${eventsById.get(id)?.account_id}` : '历史事件') }}</span><span class="caption mono muted">事件 {{ id }}</span></li></ul><div v-if="!action.results?.length" class="muted caption">尚无执行结果。</div><ul class="detail-list"><li v-for="item in action.results" :key="item.subscription_id"><span class="mono">订阅 #{{ item.subscription_id }}</span><span :class="item.success ? 'accent' : 'error-text'">{{ item.success ? '成功' : item.error || '失败' }}</span></li></ul></div></details>
        </article>
        <div v-if="!visibleActions.length" class="empty-state compact"><UiIcon name="refresh" :size="28" /><h3>暂无订阅执行记录</h3><p>自动重置或在 Telegram 确认手动重置后，执行结果会记录在这里。</p></div>
      </div>
      <div v-else class="action-list"><article v-for="delivery in visibleDeliveries" :key="delivery.id" class="action-record"><div class="action-record-heading"><div><span class="status-pill" :class="{ success: delivery.status === 'succeeded', error: delivery.status === 'failed' }">{{ statusText(delivery.status) }}</span><strong>{{ delivery.channel === 'telegram' ? 'Telegram' : '邮件' }}</strong></div><span class="caption muted">{{ dateTime(delivery.created_at) }}</span></div><p class="notification-body">{{ delivery.message }}</p><p v-if="delivery.manual_request_id" class="caption muted">此通知含手动确认按钮，请在 Telegram 内操作。请求 <span class="mono">{{ delivery.manual_request_id }}</span></p><p v-if="delivery.last_error" class="error-text">{{ delivery.last_error }}</p><p class="caption muted">尝试 {{ delivery.attempts }} 次<span v-if="['pending', 'failed'].includes(delivery.status)"> · 下次尝试 {{ dateTime(delivery.next_attempt_at) }}</span></p></article><div v-if="!visibleDeliveries.length" class="empty-state compact"><UiIcon name="bell" :size="28" /><h3>暂无通知记录</h3><p>启用渠道并在规则内选择通知方式后，事件通知会显示在这里。</p></div></div>
    </div>
    <div class="pagination"><span class="muted">{{ count }} 条记录</span><div><button class="button small secondary" type="button" :disabled="historyPage <= 1" @click="historyPage--">上一页</button><span class="mono">{{ historyPage }} / {{ pageCount }}</span><button class="button small secondary" type="button" :disabled="historyPage >= pageCount" @click="historyPage++">下一页</button></div></div>
  </section>
</template>
