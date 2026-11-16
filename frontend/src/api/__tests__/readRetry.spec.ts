import { afterEach, describe, expect, it, vi } from 'vitest'
import { retryRead } from '../readRetry'

afterEach(() => vi.useRealTimers())

describe('read query retry', () => {
  it.each([0, 408, 500, 502, 503, 504])('recovers a transient %s once', async status => {
    vi.useFakeTimers()
    const read = vi.fn().mockRejectedValueOnce({ status }).mockResolvedValue('fresh')
    const result = retryRead(read)
    await vi.advanceTimersByTimeAsync(1000)
    await expect(result).resolves.toBe('fresh')
    expect(read).toHaveBeenCalledTimes(2)
  })

  it.each([{status: 401}, {status: 403}, {status: 429}, {status: 502, code: 'OPENAI_UPSTREAM_AUTH_INVALID'}, {status: 503, retry_after_seconds: 120}])('keeps non-retryable failures', async error => {
    const read = vi.fn().mockRejectedValue(error)
    await expect(retryRead(read)).rejects.toBe(error)
    expect(read).toHaveBeenCalledTimes(1)
  })

  it('stops after the second failure', async () => {
    vi.useFakeTimers()
    const error = {status: 503}
    const read = vi.fn().mockRejectedValue(error)
    const result = expect(retryRead(read)).rejects.toBe(error)
    await vi.advanceTimersByTimeAsync(5000)
    await result
    expect(read).toHaveBeenCalledTimes(2)
  })

  it('accepts numeric API error codes without replacing the original failure', async () => {
    vi.useFakeTimers()
    const read = vi.fn().mockRejectedValueOnce({status: 503, code: 503}).mockResolvedValue('fresh')
    const result = retryRead(read)
    await vi.advanceTimersByTimeAsync(1000)
    await expect(result).resolves.toBe('fresh')
  })

  it('cancels the delayed retry when the view closes', async () => {
    vi.useFakeTimers()
    const abort = new AbortController()
    const read = vi.fn().mockRejectedValue({status: 0})
    const result = expect(retryRead(read, abort.signal)).rejects.toMatchObject({name: 'AbortError'})
    await vi.advanceTimersByTimeAsync(100)
    abort.abort()
    await vi.advanceTimersByTimeAsync(5000)
    await result
    expect(read).toHaveBeenCalledTimes(1)
  })
})
