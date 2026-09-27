<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { request } from '../api'
import { statusText } from '../format'
import type { Account, Page } from '../types'
import UiIcon from './UiIcon.vue'

const props = withDefaults(defineProps<{ modelValue: number[]; watchedIds?: number[]; compact?: boolean }>(), { watchedIds: () => [], compact: false })
const emit = defineEmits<{ 'update:modelValue': [ids: number[]]; loaded: [accounts: Account[]] }>()
const data = ref<Page<Account>>({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
const searchInput = ref('')
const groupInput = ref('')
const search = ref('')
const group = ref('')
const page = ref(1)
const loading = ref(false)
const error = ref('')
let abort: AbortController | undefined
const supported = (account: Account) => account.platform === 'openai' && account.type === 'oauth'
const selectable = computed(() => data.value.items.filter(supported))
const allSelected = computed(() => selectable.value.length > 0 && selectable.value.every((account) => props.modelValue.includes(account.id)))

async function load() {
  abort?.abort()
  const controller = new AbortController()
  abort = controller
  loading.value = true
  error.value = ''
  try {
    const query = new URLSearchParams({ page: String(page.value), page_size: '20' })
    if (search.value) query.set('search', search.value)
    if (group.value) query.set('group', group.value)
    const result = await request<Page<Account>>(`/api/accounts?${query}`, { signal: controller.signal })
    if (controller.signal.aborted) return
    data.value = { ...result, items: result.items ?? [] }
    emit('loaded', data.value.items)
  } catch (cause) {
    if (!controller.signal.aborted) error.value = cause instanceof Error ? cause.message : '账号加载失败'
  } finally {
    if (!controller.signal.aborted) loading.value = false
  }
}

function applyFilters() {
  search.value = searchInput.value.trim()
  group.value = groupInput.value.trim()
  if (page.value === 1) void load()
  else page.value = 1
}

function toggle(id: number) {
  emit('update:modelValue', props.modelValue.includes(id) ? props.modelValue.filter((value) => value !== id) : [...props.modelValue, id])
}

function togglePage() {
  const ids = new Set(props.modelValue)
  for (const account of selectable.value) {
    if (allSelected.value) ids.delete(account.id)
    else ids.add(account.id)
  }
  emit('update:modelValue', [...ids])
}

watch(page, () => void load(), { immediate: true })
onBeforeUnmount(() => abort?.abort())
</script>

<template>
  <div class="picker" :class="{ compact }">
    <form class="filter-bar" @submit.prevent="applyFilters">
      <label class="search-field">
        <span class="sr-only">搜索账号名称</span>
        <UiIcon name="search" :size="18" />
        <input v-model="searchInput" type="search" placeholder="搜索账号名称" />
      </label>
      <label class="filter-field">
        <span class="sr-only">分组 ID</span>
        <input v-model="groupInput" type="number" min="1" placeholder="分组 ID" aria-label="分组 ID" />
      </label>
      <button class="button secondary" type="submit" :disabled="loading">筛选</button>
      <button class="button icon-button" type="button" aria-label="刷新账号列表" :disabled="loading" @click="load"><UiIcon name="refresh" /></button>
    </form>
    <div class="selection-bar">
      <span>已选择 <strong class="mono">{{ modelValue.length }}</strong> 个账号<span class="muted"> · 跨页保留选择</span></span>
      <button v-if="modelValue.length" class="text-button" type="button" @click="emit('update:modelValue', [])">清空选择</button>
    </div>
    <div v-if="error" class="inline-message error" role="alert">{{ error }}<button class="text-button" type="button" @click="load">重试</button></div>
    <div class="table-scroll" :aria-busy="loading">
      <table>
        <thead><tr>
          <th class="checkbox-column"><input type="checkbox" :checked="allSelected" :disabled="!selectable.length || loading" aria-label="选择本页全部支持的账号" @change="togglePage" /></th>
          <th>账号</th><th>类型</th><th>状态</th><th>分组</th>
        </tr></thead>
        <tbody>
          <tr v-for="account in data.items" :key="account.id" :class="{ selected: modelValue.includes(account.id), subdued: !supported(account) }">
            <td><input type="checkbox" :checked="modelValue.includes(account.id)" :disabled="!supported(account) && !modelValue.includes(account.id)" :aria-label="`选择账号 ${account.name || account.id}`" @change="toggle(account.id)" /></td>
            <td><div class="table-primary">{{ account.name || `账号 #${account.id}` }}<span v-if="watchedIds.includes(account.id)" class="status-pill success">监控中</span></div><div class="muted caption mono">#{{ account.id }} · {{ account.platform }}<span v-if="!supported(account)" class="unsupported-note"> · 仅支持 OpenAI OAuth</span></div></td>
            <td><span class="status-pill">{{ account.parent_account_id ? 'Spark' : account.type === 'oauth' ? 'OAuth 主账号' : account.type }}</span></td>
            <td>{{ statusText(account.status) }}</td>
            <td class="mono muted">{{ account.group_ids?.join(', ') || '—' }}</td>
          </tr>
          <tr v-if="!data.items.length"><td colspan="5" class="table-empty">{{ loading ? '正在读取账号…' : error ? '暂时无法读取账号。请检查连接设置。' : '没有匹配的账号。' }}</td></tr>
        </tbody>
      </table>
    </div>
    <div class="pagination"><span class="muted">共 {{ data.total }} 个账号</span><div><button class="button small secondary" type="button" :disabled="page <= 1 || loading" @click="page--">上一页</button><span class="mono">{{ page }} / {{ Math.max(1, data.pages) }}</span><button class="button small secondary" type="button" :disabled="page >= Math.max(1, data.pages) || loading" @click="page++">下一页</button></div></div>
  </div>
</template>
