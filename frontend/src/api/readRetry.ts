// Opt-in for read-only queries. Upstream business failures already have their
// own server-side retry policy; this handles the browser-to-server connection.
export async function retryRead<T>(read: () => Promise<T>, signal?: AbortSignal): Promise<T> {
  try {
    return await read()
  } catch (error) {
    const failure = (error && typeof error === 'object' ? error : {}) as { status?: number; code?: unknown; retry_after_seconds?: number }
    const status = failure.status
    const transient = status === 0 || [408, 500, 502, 503, 504].includes(status ?? -1)
    if (!transient || signal?.aborted || (typeof failure.code === 'string' && failure.code.startsWith('OPENAI_'))) throw error
    const hint = failure.retry_after_seconds ?? 0
    if (hint > 2) throw error
    await new Promise<void>((resolve, reject) => {
      const abort = () => {
        clearTimeout(timer)
        reject(new DOMException('Query canceled', 'AbortError'))
      }
      const timer = setTimeout(() => {
        signal?.removeEventListener('abort', abort)
        resolve()
      }, Math.max(1000, hint * 1000))
      signal?.addEventListener('abort', abort, { once: true })
      if (signal?.aborted) abort()
    })
    return read()
  }
}
