import { onMounted, onUnmounted } from 'vue'

/** Revalidate the mounted page when the user switches back to its tab. */
export function usePageVisible(refresh: () => void) {
  const onVisibilityChange = () => {
    if (document.visibilityState === 'visible') refresh()
  }
  onMounted(() => document.addEventListener('visibilitychange', onVisibilityChange))
  onUnmounted(() => document.removeEventListener('visibilitychange', onVisibilityChange))
}
