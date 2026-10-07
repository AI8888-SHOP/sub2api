import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '@/api/client'
import OpenAISiwcLogin from '../OpenAISiwcLogin.vue'
vi.mock('@/api/client', () => ({ apiClient: { post: vi.fn(), get: vi.fn(), delete: vi.fn() } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
const credentials = { auth_mode: 'siwc', auth_flow: 'chatgpt-token-sharing', siwc_identity: 'verified', client_id: 'oaiapp_example', access_token: 'fake' }
beforeEach(() => { vi.resetAllMocks(); vi.mocked(apiClient.delete).mockResolvedValue({ data: {} }); vi.mocked(apiClient.post).mockResolvedValue({ data: { session_id: 'session', auth_url: 'https://auth.openai.com/api/accounts/authorize?state=example' } }) })
afterEach(() => vi.useRealTimers())
const encryption = { template: '<div />', mounted() { (this as unknown as { $emit: (event: string, value: boolean) => void }).$emit('ready', true) } }
function mountLogin(importCredential = vi.fn().mockResolvedValue(undefined)) { return mount(OpenAISiwcLogin, { props: { importCredential, proxyId: 7 }, global: { stubs: { CredentialEncryptionSetup: encryption } } }) }
describe('SIWC local and browser login', () => {
  it('discards a late token exchange after cancellation', async () => {
    const save = vi.fn(); const wrapper = mountLogin(save)
    await wrapper.get('select').setValue('browser'); await wrapper.get('button').trigger('click'); await flushPromises()
    await wrapper.get('input').setValue('http://localhost:8080/auth/callback?code=x')
    let complete!: (value: { data: typeof credentials }) => void
    vi.mocked(apiClient.post).mockReturnValueOnce(new Promise(resolve => { complete = resolve }))
    await wrapper.findAll('button')[0]!.trigger('click')
    await wrapper.findAll('button')[1]!.trigger('click'); await flushPromises()
    complete({ data: credentials }); await flushPromises()
    expect(save).not.toHaveBeenCalled()
    expect(wrapper.find('a').exists()).toBe(false)
  })
  it('accepts a complete browser callback and preserves registered SIWC credentials', async () => {
    const save = vi.fn().mockResolvedValue(undefined); const wrapper = mountLogin(save)
    await wrapper.get('select').setValue('browser'); await wrapper.get('button').trigger('click'); await flushPromises()
    expect(apiClient.post).toHaveBeenCalledWith('/admin/openai/siwc/sessions', { proxy_id: 7, account_id: undefined, login: undefined })
    const callback = 'http://localhost:8080/auth/callback?code=x&state=y&client_id=oaiapp_example'
    await wrapper.get('input').setValue(callback); vi.mocked(apiClient.post).mockResolvedValueOnce({ data: credentials })
    await wrapper.findAll('button')[0]!.trigger('click'); await flushPromises()
    expect(apiClient.post).toHaveBeenLastCalledWith('/admin/openai/siwc/exchange', { session_id: 'session', callback_url: callback })
    expect(save).toHaveBeenCalledWith(credentials, undefined)
  })
  it('uses the local SIWC endpoint and retains import results for retry', async () => {
    vi.useFakeTimers(); const save = vi.fn().mockRejectedValueOnce(new Error('save failed')).mockResolvedValue(undefined)
    const wrapper = mountLogin(save); await flushPromises()
    await wrapper.get('textarea').setValue('a@example.test----password----JBSWY3DPEHPK3PXP')
    await wrapper.get('button').trigger('click'); await flushPromises()
    expect(apiClient.post).toHaveBeenCalledWith('/admin/openai/siwc/sessions', { account_id: undefined, proxy_id: 7, login: { email: 'a@example.test', password: 'password', totp_secret: 'JBSWY3DPEHPK3PXP' } })
    vi.mocked(apiClient.get).mockResolvedValue({ data: { status: 'succeeded', credentials } })
    await vi.advanceTimersByTimeAsync(1500); await flushPromises()
    const retry = wrapper.findAll('button').find(b => b.text().includes('retrySave'))!; await retry.trigger('click'); await flushPromises()
    expect(apiClient.post).toHaveBeenCalledTimes(1); expect(save).toHaveBeenCalledTimes(2); expect(wrapper.get('textarea').element.value).toBe('')
  })
  it('exposes browser fallback and never imports identity-only grants', async () => {
    vi.useFakeTimers(); const save = vi.fn(); const wrapper = mountLogin(save); await flushPromises()
    await wrapper.get('textarea').setValue('a@example.test----password----JBSWY3DPEHPK3PXP'); await wrapper.get('button').trigger('click'); await flushPromises()
    vi.mocked(apiClient.get).mockResolvedValue({ data: { status: 'requires_manual' } }); await vi.advanceTimersByTimeAsync(1500); await flushPromises()
    expect(wrapper.text()).toContain('manualRequired'); expect(wrapper.get('a').attributes('href')).toContain('auth.openai.com')
    await wrapper.get('input').setValue('http://localhost:8080/auth/callback?code=x')
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { ...credentials, auth_flow: 'chatgpt-identity' } })
    await wrapper.findAll('button')[0]!.trigger('click'); await flushPromises()
    expect(save).not.toHaveBeenCalled(); expect(wrapper.get('[role="alert"]').text()).toContain('noGrant')
  })
})
