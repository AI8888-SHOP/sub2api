import { effectScope, ref } from 'vue'
import { describe, expect, it } from 'vitest'
import { usePrismCodexMode } from '../usePrismCodexMode'

describe('Prism/Codex protocol selection', () => {
  it('mutually excludes BPS synchronously in both directions and resets', () => {
    const scope = effectScope()
    scope.run(() => {
      const bps = ref(true)
      const prism = usePrismCodexMode(bps)
      prism.value = true
      expect(bps.value).toBe(false)
      bps.value = true
      expect(prism.value).toBe(false)
      bps.value = false
      expect(prism.value).toBe(false)
    })
    scope.stop()
  })
})
