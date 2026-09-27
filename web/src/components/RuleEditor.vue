<script setup lang="ts">
import { computed, ref, toRaw } from 'vue'
import { dateTime, maskText } from '../format'
import type { Account, Rule, Simulation, Subscription } from '../types'
import AccountPicker from './AccountPicker.vue'
import SubscriptionPicker from './SubscriptionPicker.vue'
import UiIcon from './UiIcon.vue'

const props = defineProps<{ rule: Rule; isNew: boolean; globalAutoReset: boolean; saving: boolean; simulating: boolean; simulation?: Simulation | null }>()
const emit = defineEmits<{ save: [rule: Rule]; simulate: [rule: Rule]; cancel: [] }>()
const draft = ref<Rule>(structuredClone(toRaw(props.rule)))
const step = ref<'rule' | 'accounts' | 'subscriptions'>('rule')
const validation = ref('')
const accounts = ref<Record<number, Account>>({})
const subscriptions = ref<Record<number, Subscription>>({})
const enabledMasks = computed(() => maskText(draft.value))

function validate(): boolean {
  validation.value = ''
  if (!draft.value.name.trim()) validation.value = '请填写规则名称。'
  else if (!draft.value.account_ids.length) validation.value = '请至少勾选一个监控账号。'
  else if (draft.value.auto_reset && !draft.value.subscription_ids.length) validation.value = '开启自动重置时，请勾选需要重置的订阅。'
  else if (draft.value.auto_reset && !draft.value.daily && !draft.value.weekly && !draft.value.monthly) validation.value = '开启自动重置时，请至少选择一种额度周期。'
  return !validation.value
}

function submit(action: 'save' | 'simulate') {
  if (!validate()) return
  const value = structuredClone(toRaw(draft.value))
  value.name = value.name.trim()
  if (action === 'save') emit('save', value)
  else emit('simulate', value)
}

function rememberAccounts(items: Account[]) {
  for (const item of items) accounts.value[item.id] = item
}

function rememberSubscriptions(items: Subscription[]) {
  for (const item of items) subscriptions.value[item.id] = item
}
</script>

<template>
  <section class="panel editor-panel" aria-labelledby="rule-editor-title">
    <div class="panel-heading"><div><span class="eyebrow">RULE EDITOR</span><h2 id="rule-editor-title">{{ isNew ? '创建联动规则' : '编辑联动规则' }}</h2></div><button class="button icon-button" type="button" aria-label="关闭规则编辑器" :disabled="saving" @click="emit('cancel')"><UiIcon name="close" /></button></div>
    <div class="editor-tabs" role="tablist" aria-label="规则配置步骤">
      <button id="rule-tab-basics" type="button" role="tab" :aria-selected="step === 'rule'" aria-controls="rule-panel-basics" :class="{ active: step === 'rule' }" @click="step = 'rule'">01 · 规则与动作</button>
      <button id="rule-tab-accounts" type="button" role="tab" :aria-selected="step === 'accounts'" aria-controls="rule-panel-accounts" :class="{ active: step === 'accounts' }" @click="step = 'accounts'">02 · 监控账号 <span class="mono">{{ draft.account_ids.length }}</span></button>
      <button id="rule-tab-subscriptions" type="button" role="tab" :aria-selected="step === 'subscriptions'" aria-controls="rule-panel-subscriptions" :class="{ active: step === 'subscriptions' }" @click="step = 'subscriptions'">03 · 关联订阅 <span class="mono">{{ draft.subscription_ids.length }}</span></button>
    </div>
    <div v-show="step === 'rule'" id="rule-panel-basics" role="tabpanel" aria-labelledby="rule-tab-basics" class="editor-body">
      <div class="form-grid">
        <label class="field"><span>规则名称</span><input v-model="draft.name" type="text" maxlength="120" placeholder="例如：主账号周额度重置 → VIP 用户" /></label>
        <label class="toggle-row compact-toggle"><span><strong>启用监控</strong><small>停用此规则后，其他启用规则仍可监控同一账号</small></span><input v-model="draft.enabled" class="switch" type="checkbox" role="switch" aria-label="启用此规则的监控" /></label>
      </div>
      <div class="rule-explainer"><div class="rule-node"><UiIcon name="users" /><span><strong>{{ draft.account_ids.length }} 个账号</strong><small>任一账号检测到 7d 重置</small></span></div><UiIcon name="arrow" /><div class="rule-node"><UiIcon name="bell" /><span><strong>重置通知</strong><small>{{ [draft.notify_telegram && 'Telegram', draft.notify_email && '邮件'].filter(Boolean).join(' + ') || '未启用通知' }}</small></span></div><UiIcon name="arrow" /><div class="rule-node"><UiIcon name="refresh" /><span><strong>{{ draft.subscription_ids.length }} 条订阅</strong><small>{{ !globalAutoReset && draft.notify_telegram && draft.subscription_ids.length && enabledMasks !== '未选择' ? `Telegram 确认 · ${enabledMasks}` : globalAutoReset && draft.auto_reset ? `自动重置${enabledMasks}` : '自动重置关闭' }}</small></span></div></div>
      <div class="form-block"><h3>通知渠道</h3><p class="muted">规则选择渠道，实际发送还需在连接设置中启用并配置相应渠道。</p><div class="checkbox-group"><label><input v-model="draft.notify_telegram" type="checkbox" /> Telegram</label><label><input v-model="draft.notify_email" type="checkbox" /> 邮件</label></div></div>
      <div class="form-block">
        <label class="toggle-row"><span><strong>自动重置关联订阅</strong><small>仅清零已勾选周期的用量并重置窗口；不会延长订阅有效期</small></span><input v-model="draft.auto_reset" class="switch" type="checkbox" role="switch" aria-label="自动重置此规则关联的订阅" /></label>
        <p class="muted caption">重置周期用于自动重置和 Telegram 手动确认。全局自动重置关闭时，无需开启上方开关。</p>
        <div class="checkbox-group"><label><input v-model="draft.daily" type="checkbox" /> 日额度</label><label><input v-model="draft.weekly" type="checkbox" /> 周额度</label><label><input v-model="draft.monthly" type="checkbox" /> 月额度</label></div>
        <div v-if="!globalAutoReset" class="inline-message muted-note"><UiIcon name="shield" :size="18" /><span>全局自动重置当前关闭。在此规则选择 Telegram、目标订阅和至少一种额度周期，并在连接设置启用 Telegram 后，归零通知内可手动选择重置或忽略。</span></div>
      </div>
      <div class="editor-next"><button class="button secondary" type="button" @click="step = 'accounts'">继续勾选监控账号 <UiIcon name="arrow" :size="17" /></button></div>
    </div>
    <div v-if="step === 'accounts'" id="rule-panel-accounts" role="tabpanel" aria-labelledby="rule-tab-accounts">
      <AccountPicker v-model="draft.account_ids" compact @loaded="rememberAccounts" />
      <div v-if="draft.account_ids.length" class="selected-summary"><span class="muted caption">已选择（点击移除）</span><div class="chips"><button v-for="id in draft.account_ids" :key="id" class="chip" type="button" :aria-label="`移除监控账号 ${accounts[id]?.name || id}`" @click="draft.account_ids = draft.account_ids.filter((value) => value !== id)">{{ accounts[id]?.name || `账号 #${id}` }} <UiIcon name="close" :size="12" /></button></div></div>
      <div class="editor-next"><button class="button secondary" type="button" @click="step = 'subscriptions'">继续关联订阅 <UiIcon name="arrow" :size="17" /></button></div>
    </div>
    <div v-if="step === 'subscriptions'" id="rule-panel-subscriptions" role="tabpanel" aria-labelledby="rule-tab-subscriptions">
      <p class="picker-note">这里选择的订阅也是 Telegram 手动确认的重置目标；通知会列出订阅 ID 与已选额度周期。</p>
      <SubscriptionPicker v-model="draft.subscription_ids" @loaded="rememberSubscriptions" />
      <div v-if="draft.subscription_ids.length" class="selected-summary"><span class="muted caption">固定关联订阅（点击移除）</span><div class="chips"><button v-for="id in draft.subscription_ids" :key="id" class="chip" type="button" :aria-label="`移除关联订阅 ${id}`" @click="draft.subscription_ids = draft.subscription_ids.filter((value) => value !== id)">{{ subscriptions[id]?.user?.email || subscriptions[id]?.user?.username || '订阅' }} #{{ id }} <UiIcon name="close" :size="12" /></button></div></div>
    </div>
    <div v-if="validation" class="inline-message error editor-message" role="alert">{{ validation }}</div>
    <div v-if="simulation" class="simulation-result">
      <div class="panel-heading"><h3><UiIcon name="shield" :size="18" /> 模拟检查结果</h3><span class="status-pill">未执行重置</span></div>
      <p>{{ simulation.message }}</p>
      <div class="simulation-counts"><span><strong class="mono">{{ simulation.accounts?.length ?? 0 }}</strong> 个账号</span><span><strong class="mono">{{ simulation.subscriptions?.length ?? 0 }}</strong> 条有效订阅</span><span><strong class="mono">{{ simulation.skipped?.length ?? 0 }}</strong> 条跳过</span><span>自动执行条件：{{ simulation.will_auto_reset ? '已开启' : '未开启' }}</span></div>
      <ul v-if="simulation.subscriptions?.length" class="detail-list"><li v-for="subscription in simulation.subscriptions" :key="subscription.id">订阅 #{{ subscription.id }} · {{ subscription.user?.email || `用户 #${subscription.user_id}` }} · 至 {{ dateTime(subscription.expires_at) }}</li></ul>
      <ul v-if="simulation.skipped?.length" class="detail-list error-text"><li v-for="item in simulation.skipped" :key="item.subscription_id">订阅 #{{ item.subscription_id }} · {{ item.error }}</li></ul>
      <p class="muted caption">模拟会读取主站数据并校验关联范围，不执行本服务的订阅重置。</p>
    </div>
    <div class="editor-footer"><button class="button secondary" type="button" :disabled="saving || simulating" @click="submit('simulate')"><UiIcon name="shield" :size="17" /> {{ simulating ? '正在检查…' : '模拟检查' }}</button><div><button class="button secondary" type="button" :disabled="saving" @click="emit('cancel')">取消</button><button class="button primary" type="button" :disabled="saving || simulating" @click="submit('save')"><UiIcon name="check" :size="18" /> {{ saving ? '正在保存…' : '保存规则' }}</button></div></div>
  </section>
</template>
