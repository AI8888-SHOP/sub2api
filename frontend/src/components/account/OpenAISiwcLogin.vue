<template>
  <div class="space-y-4" data-testid="openai-siwc-login">
    <p class="input-hint">{{ t('tokenGuard.siwc.hint') }}</p>
    <CredentialEncryptionSetup v-if="mode === 'auto' || text.trim()" @ready="encryptionReady = $event" />
    <select v-model="mode" class="input" :disabled="busy || !!session">
      <option value="auto">{{ t('tokenGuard.siwc.auto') }}</option>
      <option value="browser">{{ t('tokenGuard.siwc.browser') }}</option>
    </select>
    <label class="input-label" for="siwc-credentials">{{ t('tokenGuard.siwc.credentials') }}</label>
    <textarea id="siwc-credentials" v-model="text" class="input font-mono" rows="4" autocomplete="off" :disabled="busy || !!session" :placeholder="t('tokenGuard.twoFA.placeholder')" />
    <p class="input-hint">{{ t('tokenGuard.siwc.secretsHint') }}</p>
    <button v-if="!session" class="btn btn-primary" type="button" :disabled="busy || ((mode === 'auto' || !!text.trim()) && !encryptionReady)" @click="start">{{ t('tokenGuard.siwc.start') }}</button>
    <div v-if="authURL" class="space-y-3">
      <a :href="authURL" target="_blank" rel="noopener noreferrer" class="text-primary-600 underline">{{ t('tokenGuard.siwc.openBrowser') }}</a>
      <p class="input-hint">{{ t('tokenGuard.siwc.callbackHint') }}</p>
      <input v-model="callback" class="input" autocomplete="off" placeholder="http://localhost:8080/auth/callback?..." :disabled="busy" />
      <button type="button" class="btn btn-primary" :disabled="busy || !callback" @click="exchange">{{ t('tokenGuard.siwc.exchange') }}</button>
      <button type="button" class="btn btn-secondary ml-2" :disabled="importing" @click="cancel">{{ t('tokenGuard.siwc.cancel') }}</button>
    </div>
    <p role="status">{{ status }}</p>
    <p v-if="error" role="alert" class="text-red-600">{{ error }}</p>
    <button v-if="pendingCredentials && !busy" type="button" class="btn btn-primary" @click="save">{{ t('tokenGuard.siwc.retrySave') }}</button>
  </div>
</template>
<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import CredentialEncryptionSetup from './CredentialEncryptionSetup.vue'
import { apiClient } from '@/api/client'
import { parseTwoFALoginText, type TokenGuardReloginAccount } from '@/api/admin/accountTokenGuard'
const props = defineProps<{ proxyId?: number | null; accountId?: number; importCredential: (credentials: Record<string, unknown>, login?: TokenGuardReloginAccount) => Promise<void> }>()
const emit = defineEmits<{ busy: [boolean] }>()
const { t } = useI18n()
const encryptionReady = ref(false)
const mode = ref('auto'), text = ref(''), authURL = ref(''), session = ref(''), callback = ref('')
const error = ref(''), status = ref(''), busy = ref(false), importing = ref(false)
const pendingCredentials = ref<Record<string, unknown> | null>(null)
let queue: TokenGuardReloginAccount[] = [], current: TokenGuardReloginAccount | undefined
let timer: ReturnType<typeof setTimeout> | undefined, disposed = false, completed = 0
function setBusy(value: boolean) { busy.value = value; emit('busy', value) }
function stopPolling() { if (timer) clearTimeout(timer); timer = undefined }
async function cancel() {
  stopPolling(); const id = session.value; session.value = ''; authURL.value = ''; callback.value = ''
  pendingCredentials.value = null; current = undefined; queue = []; text.value = ''; setBusy(false)
  if (id) await apiClient.delete(`/admin/openai/siwc/sessions/${id}`).catch(() => {})
}
async function start() {
  error.value = ''; completed = 0
  if ((mode.value === 'auto' || text.value.trim()) && !encryptionReady.value) return
  try {
    queue = text.value.trim() ? parseTwoFALoginText(text.value) : []
    if (mode.value === 'auto' && !queue.length) throw new Error(t('tokenGuard.twoFA.invalid'))
    if (mode.value === 'browser' && queue.length > 1) throw new Error(t('tokenGuard.siwc.oneBrowser'))
    await next()
  } catch (e) { fail(e) }
}
async function next() {
  current = queue.shift(); setBusy(true)
  const login = mode.value === 'auto' && current ? { email: current.email, password: current.password, totp_secret: current.mfa_secret } : undefined
  try {
    const { data } = await apiClient.post('/admin/openai/siwc/sessions', { account_id: props.accountId, proxy_id: props.proxyId, login })
    if (disposed) { await apiClient.delete(`/admin/openai/siwc/sessions/${data.session_id}`); return }
    session.value = data.session_id; authURL.value = data.auth_url; status.value = t('tokenGuard.siwc.waiting')
    if (login) timer = setTimeout(poll, 1500); else setBusy(false)
  } catch (e) { fail(e) }
}
async function poll() {
  const id = session.value
  if (!id || disposed) return
  try {
    const { data } = await apiClient.get(`/admin/openai/siwc/sessions/${id}`)
    if (disposed || session.value !== id) return
    if (data.status === 'succeeded') { pendingCredentials.value = data.credentials; await save(); return }
    if (data.status === 'requires_manual') { status.value = t('tokenGuard.siwc.manualRequired'); setBusy(false); return }
    timer = setTimeout(poll, 1500)
  } catch (e) { fail(e) }
}
async function exchange() {
  const id = session.value
  error.value = ''; setBusy(true); stopPolling()
  try {
    const { data } = await apiClient.post('/admin/openai/siwc/exchange', { session_id: id, callback_url: callback.value.trim() })
    if (disposed || session.value !== id) return
    pendingCredentials.value = data; await save()
  } catch (e) { if (!disposed && session.value === id) fail(e) }
}
async function save() {
  if (!pendingCredentials.value) return
  if (pendingCredentials.value.auth_flow !== 'chatgpt-token-sharing') { error.value = t('tokenGuard.siwc.noGrant'); setBusy(false); return }
  setBusy(true); importing.value = true; error.value = ''
  try {
    await props.importCredential(pendingCredentials.value, current)
    pendingCredentials.value = null; completed++; current = undefined
    const id = session.value; session.value = ''; authURL.value = ''; callback.value = ''
    await apiClient.delete(`/admin/openai/siwc/sessions/${id}`).catch(() => {})
    status.value = t('tokenGuard.siwc.saved', { count: completed })
    if (queue.length) await next(); else { text.value = ''; setBusy(false) }
  } catch (e) { fail(e) } finally { importing.value = false }
}
function fail(e: unknown) { setBusy(false); error.value = e instanceof Error ? e.message : t('tokenGuard.siwc.failed') }
onBeforeUnmount(() => { disposed = true; void cancel() })
</script>
