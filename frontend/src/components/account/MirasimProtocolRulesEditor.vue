<template>
  <div>
    <!-- 标题与恢复默认规则按钮 -->
    <div class="mb-2 flex items-center justify-between gap-2">
      <label class="input-label mb-0">{{ t('admin.accounts.mirasim.protocolRules.title') }}</label>
      <button
        type="button"
        class="text-xs text-primary-600 hover:text-primary-700 dark:text-primary-400"
        @click="restoreDefaults"
      >
        {{ t('admin.accounts.mirasim.protocolRules.restoreDefaults') }}
      </button>
    </div>
    <p class="input-hint mb-2">{{ t('admin.accounts.mirasim.protocolRules.hint') }}</p>

    <!-- 规则列表 -->
    <div v-if="rows.length > 0" class="mb-2 space-y-2">
      <div
        v-for="(row, index) in rows"
        :key="getRowKey(row)"
        class="flex items-center gap-2"
      >
        <!-- 模型匹配 Pattern -->
        <input
          v-model="row.pattern"
          type="text"
          class="input flex-1 font-mono text-sm"
          :placeholder="t('admin.accounts.mirasim.protocolRules.patternPlaceholder')"
          :data-testid="`mirasim-protocol-pattern-${index}`"
        />
        <!-- 协议选择：Responses、Chat Completions 或 Anthropic -->
        <select
          v-model="row.protocol"
          class="input w-44 shrink-0"
          :data-testid="`mirasim-protocol-select-${index}`"
        >
          <option value="responses">
            {{ t('admin.accounts.cnProviders.apiProtocol.responses') }}
          </option>
          <option value="chat_completions">
            {{ t('admin.accounts.cnProviders.apiProtocol.chatCompletions') }}
          </option>
          <option value="anthropic">
            {{ t('admin.accounts.cnProviders.apiProtocol.anthropic') }}
          </option>
        </select>
        <!-- 删除单条规则 -->
        <button
          type="button"
          class="rounded-lg p-2 text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
          :aria-label="t('admin.accounts.mirasim.protocolRules.remove')"
          @click="removeRow(index)"
        >
          <Icon name="trash" size="sm" />
        </button>
      </div>
    </div>

    <!-- 兜底规则提示 -->
    <div
      class="mb-2 flex items-center gap-2 rounded-lg border border-dashed border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-500 dark:border-dark-600 dark:bg-dark-800/60 dark:text-gray-400"
      data-testid="mirasim-protocol-fallback"
    >
      <span class="flex-1 font-mono">*</span>
      <span>{{ t('admin.accounts.mirasim.protocolRules.fallback') }}</span>
    </div>

    <!-- 添加规则按钮 -->
    <button
      type="button"
      class="w-full rounded-lg border-2 border-dashed border-gray-300 px-4 py-2 text-gray-600 transition-colors hover:border-gray-400 hover:text-gray-700 dark:border-dark-500 dark:text-gray-400 dark:hover:border-dark-400 dark:hover:text-gray-300"
      data-testid="mirasim-protocol-add-rule"
      @click="addRow"
    >
      {{ t('admin.accounts.mirasim.protocolRules.add') }}
    </button>
  </div>
</template>

<script setup lang="ts">
/**
 * Mirasim 模型协议自适应分流规则编辑器组件
 * 允许用户针对 Mirasim 账号配置不同模型通配规则映射到 responses、chat_completions 或 anthropic 协议
 */
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { createStableObjectKeyResolver } from '@/utils/stableObjectKey'
import {
  cloneMirasimProtocolRules,
  type MirasimProtocolRule
} from '@/components/account/credentialsBuilder'

const props = defineProps<{
  rows: MirasimProtocolRule[]
}>()

const emit = defineEmits<{
  (e: 'update:rows', rows: MirasimProtocolRule[]): void
}>()

const { t } = useI18n()
const getRowKey = createStableObjectKeyResolver<MirasimProtocolRule>('mirasim-protocol-rule')

/**
 * 添加一条默认的 responses 规则
 */
const addRow = () => {
  emit('update:rows', [...props.rows, { pattern: '', protocol: 'responses' }])
}

/**
 * 移除指定索引的规则行
 */
const removeRow = (index: number) => {
  emit('update:rows', props.rows.filter((_, i) => i !== index))
}

/**
 * 恢复为默认的 Mirasim 协议规则（claude-* -> anthropic, * -> responses）
 */
const restoreDefaults = () => {
  emit('update:rows', cloneMirasimProtocolRules())
}
</script>
