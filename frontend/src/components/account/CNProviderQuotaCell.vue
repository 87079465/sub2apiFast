<template>
  <div
    v-if="visible"
    data-test="cn-provider-quota"
    class="min-w-[220px] space-y-1"
  >
    <!-- 复用账号页 UsageProgressBar 展示用量及倒计时，Mirasim 显示当期 30 天额度。 -->
    <div v-if="data?.success && data.tiers?.length" class="space-y-1">
      <UsageProgressBar
        v-for="tier in data.tiers"
        :key="tier.window"
        data-test="cn-provider-quota-tier"
        :label="windowLabel(tier.window)"
        :title="windowTooltip(tier.window)"
        :label-width="labelWidthMode"
        :color="windowColor(tier.window)"
        :utilization="tier.used_percent"
        :resets-at="tier.reset_at"
      />
      <div v-if="mirasimResetAt" class="text-[10px] leading-4 text-gray-400">
        {{ t('admin.accounts.cnProviders.window30dExpires', { date: formatDateTime(mirasimResetAt) }) }}
      </div>
    </div>

    <!-- Explicit refresh action (aligned with the OpenAI "Query" / Grok "Probe"
         buttons): a verb label tells users this chip is clickable. The previous
         noun label ("5h/weekly") read as a passive caption and users could not
         discover the manual refresh. -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="cn-provider-quota-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.probeTooltip')"
        @click="handleProbe()"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.cnProviders.probe') }}
      </button>
    </div>

    <div
      v-if="error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { CNProviderQuotaProbeResult } from '@/api/admin/cnProviders'
import type { Account } from '@/types'
import { formatDateTime } from '@/utils/format'
import { cnQuotaCellVisible } from './credentialsBuilder'
import UsageProgressBar from './UsageProgressBar.vue'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const readMode = (): string => {
  const mode = props.account.credentials?.account_mode
  return typeof mode === 'string' ? mode : ''
}

const visible = computed(() => cnQuotaCellVisible(props.account.platform, readMode()))

const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<CNProviderQuotaProbeResult | null>(null)
const mirasimResetAt = computed(() => props.account.platform === 'mirasim'
  ? data.value?.tiers?.find((tier) => tier.window === '30d')?.reset_at
  : undefined)

// 后端周期任务/手动探测写入的 extra 快照键（<provider>_ 前缀，与后端
// cnQuotaExtraUpdates 对齐）。页面加载即有数据，无需等待探测。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（模块级，跨实例共享）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

const readExtraNumber = (key: string): number | null => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

const readExtraString = (key: string): string => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'string' ? v : ''
}

// 从持久化快照构造展示数据（缺少任何已知窗口键时返回 null）。
const snapshotData = computed<CNProviderQuotaProbeResult | null>(() => {
  const platform = props.account.platform
  // 新额度只读取 30d，数据库残留的旧 5h / 7d 快照不再参与展示。
  if (platform === 'mirasim') {
    const used = readExtraNumber('mirasim_30d_used_percent')
    if (used == null) return null
    return { success: true, tiers: [{ window: '30d', used_percent: used, reset_at: readExtraString('mirasim_30d_reset_at') || undefined }] } as CNProviderQuotaProbeResult
  }
  const used5h = readExtraNumber(`${platform}_5h_used_percent`)
  const usedWeekly = readExtraNumber(`${platform}_weekly_used_percent`)
  const usedMonthly = readExtraNumber(`${platform}_monthly_used_percent`)
  const tiers: CNProviderQuotaProbeResult['tiers'] = []
  if (used5h != null) {
    tiers.push({ window: '5h', used_percent: used5h, reset_at: readExtraString(`${platform}_5h_reset_at`) || undefined })
  }
  if (usedWeekly != null) {
    tiers.push({ window: 'weekly', used_percent: usedWeekly, reset_at: readExtraString(`${platform}_weekly_reset_at`) || undefined })
  }
  if (usedMonthly != null) {
    tiers.push({ window: 'monthly', used_percent: usedMonthly, reset_at: readExtraString(`${platform}_monthly_reset_at`) || undefined })
  }
  if (tiers.length === 0) return null
  return { success: true, tiers } as CNProviderQuotaProbeResult
})

// 快照是否过期（无更新时间或超过 staleness 窗口）→ 挂载时需要自动探测。
const snapshotIsStale = computed(() => {
  const updatedAt = readExtraString(`${props.account.platform}_usage_updated_at`)
  if (!updatedAt) return true
  const ts = new Date(updatedAt).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次（失败显示错误，
// 避免静默失败导致单元格空白无提示）。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
  if (snapshotData.value && !snapshotIsStale.value) return
  // 模块级去抖：列表页每行一个实例，翻页/筛选/刷新会重复挂载；同一账号
  // 短时间内已自动探测过则跳过，避免对上游形成探测风暴。
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

// 标签徽章宽度：Mirasim 的「30 天额度」使用 wide 加宽定宽
// （同格及跨行进度条纵向对齐）；其余 CN 平台标签均为短标签，保持默认 fixed。
const labelWidthMode = computed<'fixed' | 'wide'>(() =>
  props.account.platform === 'mirasim' ? 'wide' : 'fixed'
)

/**
 * 窗口名 → 徽章展示文案。
 * @param window 后端窗口名（5h / weekly / monthly / Mirasim 的 30d）
 * @returns 本地化后的短标签；未知的 Mirasim 扩展窗口原样透传
 */
const windowLabel = (window: string) => {
  if (window === 'weekly' || window === '7d') return t('admin.accounts.cnProviders.windowWeekly')
  if (window === 'monthly') return t('admin.accounts.cnProviders.windowMonthly')
  if (window === '30d') return t('admin.accounts.cnProviders.window30d')
  if (window === '5h') return t('admin.accounts.cnProviders.window5h')
  return window
}

/**
 * 窗口名 → 悬浮说明（解释窗口含义及计入范围）。
 * @param window 后端窗口名
 * @returns 本地化说明文案；未知窗口返回原窗口名
 */
const windowTooltip = (window: string) => {
  if (window === '5h') return t('admin.accounts.cnProviders.window5hTooltip')
  if (window === 'weekly' || window === '7d') return t('admin.accounts.cnProviders.windowWeeklyTooltip')
  if (window === 'monthly') return t('admin.accounts.cnProviders.windowMonthlyTooltip')
  if (window === '30d') return t('admin.accounts.cnProviders.window30dTooltip')
  return window
}

/**
 * 窗口名 → 徽章配色：5h/月及 30 天额度使用靛蓝，周窗口使用翠绿。
 * @param window 后端窗口名
 * @returns UsageProgressBar 支持的色板名
 */
const windowColor = (window: string): 'indigo' | 'emerald' | 'purple' | 'amber' => {
  if (window === 'weekly' || window.startsWith('7d')) return 'emerald'
  return 'indigo'
}

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.cnProviders.queryQuota(props.account.id)
    // 失败时保留已渲染的快照条形图（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
  }
)
</script>
