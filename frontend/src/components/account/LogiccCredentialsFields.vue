<template>
  <div class="space-y-3 rounded-lg border border-cyan-200 p-4 dark:border-cyan-900">
    <div class="rounded-lg bg-cyan-50 p-3 dark:bg-cyan-950/30">
      <div class="flex flex-wrap items-center gap-3">
        <button type="button" class="btn btn-primary" data-test="logicc-browser-import" @click="browserImport.start">
          {{ t('admin.accounts.logicc.browserImport') }}
        </button>
        <a href="/downloads/logicc-browser-import.zip" download class="text-sm text-cyan-700 underline dark:text-cyan-300">
          {{ t('admin.accounts.logicc.downloadExtension') }}
        </a>
      </div>
      <p class="input-hint mt-2">{{ t('admin.accounts.logicc.browserImportHint') }}</p>
      <p v-if="browserImport.status.value !== 'idle'" role="status" class="mt-2 text-sm" data-test="logicc-import-status">
        {{ t(`admin.accounts.logicc.importStatus.${browserImport.status.value}`) }}
      </p>
      <button v-if="browserImport.status.value === 'waiting'" type="button" class="mt-2 text-sm underline" @click="browserImport.cancel">
        {{ t('admin.accounts.logicc.cancelImport') }}
      </button>
      <span
        v-if="browserImport.nonce.value"
        hidden
        data-sub2api-logicc-import="v1"
        :data-nonce="browserImport.nonce.value"
        :data-expires-at="browserImport.expiresAt.value"
      />
      <details class="mt-2 text-sm">
        <summary class="cursor-pointer text-cyan-700 dark:text-cyan-300">{{ t('admin.accounts.logicc.installHelp') }}</summary>
        <ol class="mt-2 list-decimal space-y-1 pl-5 text-gray-600 dark:text-gray-400">
          <li>{{ t('admin.accounts.logicc.installStep1') }}</li>
          <li>{{ t('admin.accounts.logicc.installStep2') }}</li>
          <li>{{ t('admin.accounts.logicc.installStep3') }}</li>
          <li>{{ t('admin.accounts.logicc.installStep4') }}</li>
        </ol>
      </details>
    </div>
    <p class="input-hint">{{ t('admin.accounts.logicc.hint') }}</p>
    <p v-if="editing" class="input-hint">{{ t('admin.accounts.logicc.editingHint') }}</p>
    <div v-for="key in keys" :key="key">
      <label :for="`logicc-${key}`" class="input-label">{{ t(`admin.accounts.logicc.${key}`) }}</label>
      <input
        :id="`logicc-${key}`"
        :value="modelValue[key] || ''"
        :type="key === 'access_token' || key === 'client_cookie' ? 'password' : 'text'"
        class="input"
        autocomplete="off"
        @input="update(key, $event)"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useLogiccBrowserImport } from '@/composables/useLogiccBrowserImport'
const { t } = useI18n()
const props = defineProps<{ modelValue: Record<string, string>; editing?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: Record<string, string>] }>()
const browserImport = useLogiccBrowserImport(credentials => {
  emit('update:modelValue', { ...props.modelValue, ...credentials })
})
const keys = ['access_token', 'client_cookie', 'session_id', 'assistant_id'] as const
function update(key: string, event: Event) {
  emit('update:modelValue', { ...props.modelValue, [key]: (event.target as HTMLInputElement).value })
}
</script>
