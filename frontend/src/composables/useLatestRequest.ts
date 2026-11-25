import { onScopeDispose } from 'vue'

/** Each new read supersedes the previous read, including its error and loading state. */
export function useLatestRequest() {
  let generation = 0
  let disposed = false
  onScopeDispose(() => { disposed = true })
  return () => {
    const current = ++generation
    return () => !disposed && current === generation
  }
}
