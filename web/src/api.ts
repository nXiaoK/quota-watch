export class ApiError extends Error {
  constructor(message: string, public readonly status: number) {
    super(message)
    this.name = 'ApiError'
  }
}

const activeRequests = new Set<AbortController>()
let requestGeneration = 0
let authenticationRequired: (() => void) | undefined

export function onAuthenticationRequired(handler: () => void) {
  authenticationRequired = handler
  return () => { if (authenticationRequired === handler) authenticationRequired = undefined }
}

export function cancelSessionRequests() {
  requestGeneration += 1
  for (const controller of activeRequests) controller.abort()
  activeRequests.clear()
}

export async function request<T>(path: string, options: { method?: string; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const generation = requestGeneration
  const controller = new AbortController()
  const abort = () => controller.abort()
  if (options.signal?.aborted) abort()
  else options.signal?.addEventListener('abort', abort, { once: true })
  activeRequests.add(controller)
  try {
    const response = await fetch(path, {
      method,
      credentials: 'same-origin',
      signal: controller.signal,
      headers: method === 'GET' ? {} : { 'Content-Type': 'application/json', 'X-Quota-Watch': '1' },
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    })
    const result = await response.json().catch(() => null)
    if (controller.signal.aborted || generation !== requestGeneration) throw new DOMException('会话请求已取消', 'AbortError')
    if (!response.ok) {
      if (response.status === 401 && !path.startsWith('/api/auth/')) authenticationRequired?.()
      throw new ApiError(typeof result?.error === 'string' ? result.error : `请求失败（HTTP ${response.status}）`, response.status)
    }
    if (result === null) throw new Error('服务返回了无效数据，请检查连接后重试。')
    return result as T
  } finally {
    activeRequests.delete(controller)
    options.signal?.removeEventListener('abort', abort)
  }
}
