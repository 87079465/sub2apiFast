import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LogiccCredentialsFields from '@/components/account/LogiccCredentialsFields.vue'
import { LOGICC_IMPORT_MESSAGE, validateLogiccImport } from '../useLogiccBrowserImport'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function credentials(session = 'sess_example', expiry = Math.floor(Date.now() / 1000) + 300) {
  const encoded = btoa(JSON.stringify({ sid: session, exp: expiry })).replace(/=/g, '')
  return { access_token: `e30.${encoded}.signature`, client_cookie: 'sample-cookie', session_id: session }
}

function send(nonce: string, value: unknown, overrides: Partial<MessageEventInit> = {}) {
  window.dispatchEvent(new MessageEvent('message', {
    source: window,
    origin: window.location.origin,
    data: { type: LOGICC_IMPORT_MESSAGE, nonce, credentials: value },
    ...overrides
  }))
}

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

describe('Logicc browser import', () => {
  it('accepts a fresh matching session and rejects wrong-session, expired and malformed credentials', () => {
    expect(validateLogiccImport(credentials())).toEqual(credentials())
    expect(validateLogiccImport({ ...credentials(), session_id: 'sess_other' })).toBeNull()
    expect(validateLogiccImport(credentials('sess_example', 1))).toBeNull()
    expect(validateLogiccImport({ ...credentials(), client_cookie: '__client=value; other=value' })).toBeNull()
    expect(validateLogiccImport(null)).toBeNull()
  })

  it('fills one armed form once, preserving assistant ID and requiring the correct nonce and origin', async () => {
    const wrapper = mount(LogiccCredentialsFields, { props: { modelValue: { assistant_id: 'assistant_keep' } } })
    await wrapper.get('[data-test="logicc-browser-import"]').trigger('click')
    const nonce = wrapper.get('[data-sub2api-logicc-import]').attributes('data-nonce')
    send('wrong-nonce', credentials())
    send(nonce, credentials(), { origin: 'https://another.example.com' })
    send(nonce, credentials(), { source: null })
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    const post = vi.spyOn(window, 'postMessage').mockImplementation(() => {})
    send(nonce, credentials())
    await nextTick()
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ assistant_id: 'assistant_keep', ...credentials() }])
    expect(wrapper.find('[data-sub2api-logicc-import]').exists()).toBe(false)
    expect(wrapper.get('[role="status"]').text()).toContain('success')
    expect(JSON.stringify(post.mock.calls)).not.toContain('sample-cookie')
    send(nonce, credentials())
    expect(wrapper.emitted('update:modelValue')).toHaveLength(1)
    wrapper.unmount()
  })

  it('expires the import and clears it when the form closes', async () => {
    vi.useFakeTimers()
    const wrapper = mount(LogiccCredentialsFields, { props: { modelValue: {} } })
    await wrapper.get('[data-test="logicc-browser-import"]').trigger('click')
    const nonce = wrapper.get('[data-sub2api-logicc-import]').attributes('data-nonce')
    await vi.advanceTimersByTimeAsync(120_000)
    send(nonce, credentials())
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.get('[role="status"]').text()).toContain('expired')
    await wrapper.get('[data-test="logicc-browser-import"]').trigger('click')
    const nextNonce = wrapper.get('[data-sub2api-logicc-import]').attributes('data-nonce')
    wrapper.unmount()
    send(nextNonce, credentials())
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects invalid credentials and allows restarting the import', async () => {
    const wrapper = mount(LogiccCredentialsFields, { props: { modelValue: {} } })
    await wrapper.get('[data-test="logicc-browser-import"]').trigger('click')
    const nonce = wrapper.get('[data-sub2api-logicc-import]').attributes('data-nonce')
    send(nonce, credentials('sess_example', 1))
    await nextTick()
    expect(wrapper.get('[role="status"]').text()).toContain('invalid')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await wrapper.get('[data-test="logicc-browser-import"]').trigger('click')
    expect(wrapper.get('[data-sub2api-logicc-import]').attributes('data-nonce')).not.toBe(nonce)
    wrapper.unmount()
  })
})
