import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OfficeCopilotUsageCell from '../OfficeCopilotUsageCell.vue'
vi.mock('vue-i18n', async () => {
  const { default: messages } = await import('@/i18n/locales/zh/admin/accounts')
  return { useI18n: () => ({ t: (key: string, values: Record<string, unknown> = {}) => {
    const name = key.split('.').pop() as keyof typeof messages.accounts.officeQuota
    return messages.accounts.officeQuota[name].replace(/\{(\w+)\}/g, (_, field) => String(values[field] ?? ''))
  } }) }
})

describe('OfficeCopilotUsageCell', () => {
  it('shows capability restrictions without treating them as account exhaustion', async () => {
    const wrapper = mount(OfficeCopilotUsageCell, { props: { snapshot: {
      observed_at: '2026-10-13T00:00:00Z', model: 'claude-opus-5-5',
      restrictions: [{ code: 'ImageGenInsufficientTokensThrottled', category: 'quota', scope: 'image_generation', has_access: false }]
    } } })
    expect(wrapper.get('[data-test="office-quota-restriction"]').text()).toContain('不代表文本模型额度耗尽')
    expect(wrapper.get('[data-test="office-quota-status"]').text()).toContain('上游未提供')
    expect(wrapper.text()).toContain('非实时余额')
    await wrapper.setProps({ snapshot: {
      observed_at: '2026-10-13T00:00:00Z', model: 'claude-opus-5-5',
      restrictions: [{ code: 'ImageGenSystemCapacityThrottled', category: 'capacity', scope: 'image_generation' }]
    } })
    expect(wrapper.get('[data-test="office-quota-restriction"]').text()).toContain('上游容量限制')
    await wrapper.setProps({ snapshot: null })
    expect(wrapper.find('[data-test="office-quota-restriction"]').exists()).toBe(false)
  })
  it('does not turn the live Opus response containing GPT zeros and 1/600 into account quota', () => {
    const wrapper = mount(OfficeCopilotUsageCell, { props: { snapshot: {
      observed_at: '2026-10-13T00:00:00Z', model: 'claude-opus-5-5',
      conversation_used: 1, conversation_limit: 600,
      meters: {
        GPT61SolQueryWeekly: { remainingAllowance: 0 },
        GPT61SolQueryDailyWord: { remainingAllowance: 0 },
        GPT61SolQueryWeeklyWord: { remainingAllowance: 0 },
        GPT61SolQueryDaily: { remainingAllowance: 0 }
      }
    } } })
    expect(wrapper.get('[data-test="office-quota-status"]').text()).toContain('上游未提供')
    expect(wrapper.findAll('[data-test="office-quota-meter"]')).toHaveLength(0)
    expect(wrapper.text()).not.toContain('Opus 5.5 · 日')
    expect(wrapper.text()).not.toContain('Sonnet 5')
    expect(wrapper.text()).toContain('1 / 600 条（非账号额度）')
    expect(wrapper.text()).toContain('每次 API 请求会新建上游会话')
    expect(wrapper.get('details').attributes('open')).toBeUndefined()
    expect(wrapper.text()).toContain('claude-opus-5-5')
    expect(wrapper.text()).toContain('非实时余额')
  })

  it('keeps distinct counters in diagnostics without inventing an applicable balance', async () => {
    const wrapper = mount(OfficeCopilotUsageCell, { props: { snapshot: {
      observed_at: '2026-10-13T00:00:00Z', model: 'claude-opus-5-5', meters: {
        ClaudeOpusQueryDailyWord: { remainingAllowance: 0 },
        ClaudeOpusQueryDaily: { remainingAllowance: 10 },
        ClaudeOpusQuery75: { remainingAllowance: 9 },
        ClaudeOpusQueryWeeklyWord: { remainingAllowance: 8 }
      }
    } } })
    expect(wrapper.get('[data-test="office-quota-status"]').text()).toContain('无法确认')
    const rows = wrapper.findAll('[data-test="office-quota-meter"]').map(row => row.text())
    expect(rows).toEqual(['ClaudeOpusQuery75: 9', 'ClaudeOpusQueryWeeklyWord: 8', 'ClaudeOpusQueryDailyWord: 0', 'ClaudeOpusQueryDaily: 10'])
    expect(wrapper.text()).not.toContain('剩余 0')
    expect(wrapper.text()).toContain('不代表可用请求次数')
    await wrapper.get('button').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    await wrapper.setProps({ snapshot: null })
    expect(wrapper.find('details').exists()).toBe(false)
    expect(wrapper.text()).toContain('暂无有效记录')
  })

  it('ignores legacy empty failures and invalid meter numbers', () => {
    for (const meters of [undefined, { ClaudeOpusQueryDailyWord: { remainingAllowance: -1 } },
      { ClaudeOpusQueryDailyWord: { remainingAllowance: Number.NaN } }]) {
      const wrapper = mount(OfficeCopilotUsageCell, { props: { snapshot: {
        observed_at: '2026-10-13T00:00:00Z', model: 'gpt-6-sol', meters
      } } })
      expect(wrapper.text()).toContain('暂无有效记录')
      expect(wrapper.find('details').exists()).toBe(false)
      expect(wrapper.text()).not.toContain('记录于')
      wrapper.unmount()
    }
  })
})
