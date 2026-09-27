export function dateTime(value?: string | number | null): string {
  if (!value || (typeof value === 'string' && value.startsWith('0001-'))) return '—'
  const date = new Date(typeof value === 'number' ? value * 1000 : value)
  if (Number.isNaN(date.getTime())) return '—'
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function percent(value?: number | null): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${Number(value.toFixed(3))}%` : '—'
}

export function dollars(value: number): string {
  return Number.isFinite(value) ? `$${value.toFixed(2)}` : '—'
}

export function statusText(value: string): string {
  return ({
    active: '有效', inactive: '未启用', disabled: '已停用', error: '异常', healthy: '正常', ok: '正常',
    watching: '监控中', baseline: '已建立基线', pending: '等待处理', pending_confirmation: '等待复核',
    confirming: '复核中', reset: '检测到重置', unsupported: '不支持', revoked: '已撤销', expired: '已过期',
    success: '成功', succeeded: '成功', sent: '已发送', failed: '失败', partial: '部分失败',
    skipped: '已跳过', running: '执行中', retrying: '等待重试', retried: '已重试', uncertain: '结果待核对',
    unknown: '结果未知', paused: '已暂停', initial: '等待首次快照检查', waiting: '等待快照更新',
    processing: '执行中', completed: '已完成', ignored: '已忽略', invalid: '已失效', invalidated: '已失效', superseded: '已被新请求替代',
    conflict: '接收冲突', webhook: 'Webhook 占用', listening: '正在接收', polling: '接收中', idle: '等待确认请求',
  } as Record<string, string>)[value] ?? value ?? '未知'
}

export function maskText(mask: { daily: boolean; weekly: boolean; monthly: boolean }): string {
  return [mask.daily && '日额度', mask.weekly && '周额度', mask.monthly && '月额度'].filter(Boolean).join(' / ') || '未选择'
}

export function subscriptionIsActive(subscription: { status: string; starts_at: string; expires_at: string }): boolean {
  const now = Date.now()
  return subscription.status === 'active' && new Date(subscription.starts_at).getTime() <= now && new Date(subscription.expires_at).getTime() > now
}
