<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { request } from '../api'
import { dateTime, dollars, statusText, subscriptionIsActive } from '../format'
import type { Page, Subscription } from '../types'
import UiIcon from './UiIcon.vue'

const props = defineProps<{ modelValue: number[] }>()
const emit = defineEmits<{ 'update:modelValue': [ids: number[]]; loaded: [subscriptions: Subscription[]] }>()
const data = ref<Page<Subscription>>({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
const userInput = ref('')
const groupInput = ref('')
const userId = ref('')
const groupId = ref('')
const search = ref('')
const page = ref(1)
const loading = ref(false)
const error = ref('')
let abort: AbortController | undefined
const visible = computed(() => {
  const text = search.value.trim().toLowerCase()
  return text ? data.value.items.filter((subscription) => `${subscription.id} ${subscription.user_id} ${subscription.user?.email ?? ''} ${subscription.user?.username ?? ''} ${subscription.group?.name ?? ''}`.toLowerCase().includes(text)) : data.value.items
})
const eligible = computed(() => visible.value.filter(subscriptionIsActive))
const allSelected = computed(() => eligible.value.length > 0 && eligible.value.every((subscription) => props.modelValue.includes(subscription.id)))

async function load() {
  abort?.abort()
  const controller = new AbortController()
  abort = controller
  loading.value = true
  error.value = ''
  try {
    const query = new URLSearchParams({ page: String(page.value), page_size: '20' })
    if (userId.value) query.set('user_id', userId.value)
    if (groupId.value) query.set('group_id', groupId.value)
    const result = await request<Page<Subscription>>(`/api/subscriptions?${query}`, { signal: controller.signal })
    if (controller.signal.aborted) return
    data.value = { ...result, items: result.items ?? [] }
    emit('loaded', data.value.items)
  } catch (cause) {
    if (!controller.signal.aborted) error.value = cause instanceof Error ? cause.message : '订阅加载失败'
  } finally {
    if (!controller.signal.aborted) loading.value = false
  }
}

function applyFilters() {
  userId.value = userInput.value.trim()
  groupId.value = groupInput.value.trim()
  if (page.value === 1) void load()
  else page.value = 1
}

function toggle(id: number) {
  emit('update:modelValue', props.modelValue.includes(id) ? props.modelValue.filter((value) => value !== id) : [...props.modelValue, id])
}

function togglePage() {
  const ids = new Set(props.modelValue)
  for (const subscription of eligible.value) {
    if (allSelected.value) ids.delete(subscription.id)
    else ids.add(subscription.id)
  }
  emit('update:modelValue', [...ids])
}

watch(page, () => void load(), { immediate: true })
onBeforeUnmount(() => abort?.abort())
</script>

<template>
  <div class="picker compact">
    <form class="filter-bar" @submit.prevent="applyFilters">
      <label class="filter-field"><span class="sr-only">按用户 ID 查询</span><input v-model="userInput" type="number" min="1" placeholder="用户 ID" aria-label="用户 ID" /></label>
      <label class="filter-field"><span class="sr-only">按分组 ID 查询</span><input v-model="groupInput" type="number" min="1" placeholder="分组 ID" aria-label="订阅分组 ID" /></label>
      <button class="button secondary" type="submit" :disabled="loading">查询订阅</button>
      <button class="button icon-button" type="button" aria-label="刷新订阅列表" :disabled="loading" @click="load"><UiIcon name="refresh" /></button>
    </form>
    <label class="search-field subscription-search"><span class="sr-only">筛选当前页订阅</span><UiIcon name="search" :size="18" /><input v-model="search" type="search" placeholder="筛选当前页：邮箱、用户名、订阅 ID 或分组名称" /></label>
    <div class="selection-bar"><span>已选择 <strong class="mono">{{ modelValue.length }}</strong> 条订阅<span class="muted"> · 跨页保留选择</span></span><button v-if="modelValue.length" class="text-button" type="button" @click="emit('update:modelValue', [])">清空选择</button></div>
    <p class="muted caption picker-note">仅可新增选择当前有效的订阅。保存后固定关联订阅 ID，新订阅需重新勾选。</p>
    <div v-if="error" class="inline-message error" role="alert">{{ error }}<button class="text-button" type="button" @click="load">重试</button></div>
    <div class="table-scroll" :aria-busy="loading"><table>
      <thead><tr><th class="checkbox-column"><input type="checkbox" :checked="allSelected" :disabled="!eligible.length || loading" aria-label="选择本页所有有效订阅" @change="togglePage" /></th><th>用户 / 订阅</th><th>分组</th><th>状态 / 有效期</th><th>周用量</th></tr></thead>
      <tbody>
        <tr v-for="subscription in visible" :key="subscription.id" :class="{ selected: modelValue.includes(subscription.id), subdued: !subscriptionIsActive(subscription) }">
          <td><input type="checkbox" :checked="modelValue.includes(subscription.id)" :disabled="!subscriptionIsActive(subscription) && !modelValue.includes(subscription.id)" :aria-label="`选择订阅 ${subscription.id}`" @change="toggle(subscription.id)" /></td>
          <td><div class="table-primary">{{ subscription.user?.username || subscription.user?.email || `用户 #${subscription.user_id}` }}</div><div v-if="subscription.user?.username && subscription.user?.email" class="caption muted">{{ subscription.user.email }}</div><div class="caption muted mono">订阅 #{{ subscription.id }} · 用户 #{{ subscription.user_id }}</div></td>
          <td>{{ subscription.group?.name || `分组 #${subscription.group_id}` }}<div class="caption muted mono">#{{ subscription.group_id }}</div></td>
          <td><span class="status-pill" :class="{ success: subscriptionIsActive(subscription) }">{{ subscriptionIsActive(subscription) ? '有效' : statusText(subscription.status) === '有效' ? '不在有效期内' : statusText(subscription.status) }}</span><div class="caption muted">{{ dateTime(subscription.starts_at) }}<br />至 {{ dateTime(subscription.expires_at) }}</div></td>
          <td class="mono">{{ dollars(subscription.weekly_usage_usd) }}</td>
        </tr>
        <tr v-if="!visible.length"><td colspan="5" class="table-empty">{{ loading ? '正在读取订阅…' : error ? '暂时无法读取订阅。' : '没有匹配的订阅。可调整用户或分组条件。' }}</td></tr>
      </tbody>
    </table></div>
    <div class="pagination"><span class="muted">共 {{ data.total }} 条订阅 · 当前页显示 {{ visible.length }} 条</span><div><button class="button small secondary" type="button" :disabled="page <= 1 || loading" @click="page--">上一页</button><span class="mono">{{ page }} / {{ Math.max(1, data.pages) }}</span><button class="button small secondary" type="button" :disabled="page >= Math.max(1, data.pages) || loading" @click="page++">下一页</button></div></div>
  </div>
</template>
