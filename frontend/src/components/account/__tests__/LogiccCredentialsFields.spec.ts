import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import LogiccCredentialsFields from '../LogiccCredentialsFields.vue'
import { CONCRETE_PLATFORM_OPTIONS, GROUP_PLATFORM_OPTIONS } from '@/constants/platforms'
import { platformLabel } from '@/utils/platformColors'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Logicc account setup', () => {
  it('allows selecting Logicc for both accounts and groups', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.some(p => p.value === 'logicc')).toBe(true)
    expect(GROUP_PLATFORM_OPTIONS.some(p => p.value === 'logicc')).toBe(true)
    expect(platformLabel('logicc')).toBe('Logicc')
  })

  it('masks credentials and preserves other input when editing one field', async () => {
    const wrapper = mount(LogiccCredentialsFields, { props: { modelValue: { session_id: 'sess_test' }, editing: true } })
    expect(wrapper.get('#logicc-access_token').attributes('type')).toBe('password')
    expect(wrapper.get('#logicc-client_cookie').attributes('type')).toBe('password')
    await wrapper.get('#logicc-client_cookie').setValue('cookie-value')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ session_id: 'sess_test', client_cookie: 'cookie-value' }])
    expect(wrapper.text()).toContain('editingHint')
  })
})
