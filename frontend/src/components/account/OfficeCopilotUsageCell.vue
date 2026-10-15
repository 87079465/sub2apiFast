<template>
  <div class="space-y-1 text-xs" data-test="office-copilot-usage">
    <div class="flex items-center justify-between gap-3" data-test="office-quota-status">
      <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.officeQuota.accountBalance') }}</span>
      <span class="text-gray-700 dark:text-gray-200">{{ t(rows.length ? 'admin.accounts.officeQuota.unverified' : 'admin.accounts.officeQuota.unavailable') }}</span>
    </div>
    <div class="max-w-[250px] text-gray-400">{{ t('admin.accounts.officeQuota.balanceHelp') }}</div>
    <div v-for="restriction in validSnapshot?.restrictions ?? []" :key="restriction.code" class="max-w-[250px] text-amber-600 dark:text-amber-400" data-test="office-quota-restriction">
      {{ t('admin.accounts.officeQuota.restrictionObserved') }}{{ restrictionText(restriction.code) }}
    </div>
    <details v-if="validSnapshot" class="max-w-[250px] text-gray-500 dark:text-gray-400">
      <summary class="cursor-pointer">{{ t('admin.accounts.officeQuota.details') }}</summary>
      <div class="mt-1 space-y-1">
        <div>{{ t('admin.accounts.officeQuota.requestModel', { model: validSnapshot.model }) }}</div>
        <div v-if="validSnapshot.conversation_limit != null">
          {{ t('admin.accounts.officeQuota.conversation', { used: validSnapshot.conversation_used ?? '?', limit: validSnapshot.conversation_limit }) }}
        </div>
        <div>{{ t('admin.accounts.officeQuota.sessionHelp') }}</div>
        <template v-if="rows.length">
          <div>{{ t('admin.accounts.officeQuota.meterHelp') }}</div>
          <div v-for="row in rows" :key="row.key" class="break-all" data-test="office-quota-meter">
            {{ row.key }}: {{ row.remaining }}
          </div>
        </template>
      </div>
    </details>
    <div v-if="error" class="text-red-500">{{ error }}</div>
    <div class="flex items-center gap-2 text-gray-400">
      <span :title="t('admin.accounts.officeQuota.help')">{{ t('admin.accounts.officeQuota.snapshot') }}</span>
      <button type="button" class="text-primary-500 hover:underline disabled:opacity-50" :disabled="loading" :title="t('admin.accounts.officeQuota.help')" @click="$emit('refresh')">
        {{ t(loading ? 'admin.accounts.officeQuota.loading' : 'admin.accounts.officeQuota.refresh') }}
      </button>
    </div>
    <div class="max-w-[250px] text-gray-400">
      {{ validSnapshot ? t('admin.accounts.officeQuota.observed', { time: observedTime }) : t('admin.accounts.officeQuota.noSnapshot') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OfficeCopilotQuotaSnapshot } from '@/types'

const props = defineProps<{ snapshot?: OfficeCopilotQuotaSnapshot | null; loading?: boolean; error?: string | null }>()
defineEmits<{ refresh: [] }>()
const { t } = useI18n()

// Older servers stamped even empty failed streams. Do not show those as a
// successful observation, or present unrelated GPT counters as Claude balance.
const meterKeys = ['ClaudeOpusQuery75', 'ClaudeOpusQueryWeeklyWord', 'ClaudeOpusQueryDailyWord', 'ClaudeOpusQueryDaily', 'ClaudeOpusQueryDev']
const rows = computed(() => meterKeys.flatMap(key => {
  const value = props.snapshot?.meters?.[key]?.remainingAllowance
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
    ? [{ key, remaining: value }] : []
}))
const validSnapshot = computed(() => {
  const snapshot = props.snapshot
  if (!snapshot || !Number.isFinite(new Date(snapshot.observed_at).getTime())) return null
  return rows.value.length || snapshot.restrictions?.length || (Number.isInteger(snapshot.conversation_limit) && snapshot.conversation_limit! > 0)
    || (Number.isInteger(snapshot.conversation_used) && snapshot.conversation_used! >= 0) ? snapshot : null
})
const observedTime = computed(() => new Date(validSnapshot.value?.observed_at ?? '').toLocaleString())
function restrictionText(code: string) {
  if (code === 'ImageGenInsufficientTokensThrottled') return t('admin.accounts.officeQuota.imageQuotaLimited')
  if (code === 'ImageGenSystemCapacityThrottled') return t('admin.accounts.officeQuota.imageCapacityLimited')
  return t('admin.accounts.officeQuota.capabilityRestricted')
}
</script>
