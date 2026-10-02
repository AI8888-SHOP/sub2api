<template>
  <div class="border-t border-gray-200 pt-4 dark:border-dark-600" data-testid="prism-codex-settings">
    <label class="flex items-center gap-2">
      <input v-model="enabled" type="checkbox" role="switch" data-testid="prism-codex-toggle" />
      <span class="text-sm font-medium">{{ t('admin.accounts.openai.prismCodex') }}</span>
    </label>
    <p class="input-hint">{{ t('admin.accounts.openai.prismCodexDesc') }}</p>
    <p v-if="enabled" class="mt-2 text-xs text-amber-600 dark:text-amber-400">
      {{ t('admin.accounts.openai.prismCodexScope') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const enabled = defineModel<boolean>({ default: false })
const { t } = useI18n()
</script>
