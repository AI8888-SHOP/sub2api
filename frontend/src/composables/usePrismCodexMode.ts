import { ref, watch, type Ref } from 'vue'

// Synchronous mutual exclusion also covers programmatic/defaults updates.
export function usePrismCodexMode(bps: Ref<boolean>) {
  const enabled = ref(false)
  watch(enabled, value => { if (value) bps.value = false }, { flush: 'sync' })
  watch(bps, value => { if (value) enabled.value = false }, { flush: 'sync' })
  return enabled
}
