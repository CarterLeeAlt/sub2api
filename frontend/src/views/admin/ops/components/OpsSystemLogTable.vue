<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useMediaQuery } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import LogRetentionSelect from './LogRetentionSelect.vue'
import { opsAPI, type OpsRuntimeLogConfig, type OpsSystemLog, type OpsSystemLogCleanupRequest, type OpsSystemLogQuery, type OpsSystemLogSinkHealth } from '@/api/admin/ops'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'
import { DEFAULT_OPS_TIME_RANGE } from '../utils/opsTimeRange'

const appStore = useAppStore()
const { t } = useI18n()

// 与 DataTable 一致：< 768px 切换为卡片视图，避免宽表在移动端被截断。
const isDesktopViewport = useMediaQuery('(min-width: 768px)')

const props = withDefaults(defineProps<{
  platformFilter?: string
  refreshToken?: number
}>(), {
  platformFilter: '',
  refreshToken: 0
})

const loading = ref(false)
const logs = ref<OpsSystemLog[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const cleanupScope = ref<OpsSystemLogCleanupRequest | null>(null)
const cleaning = ref(false)
const listFailed = ref(false)
let listRequest = 0

const health = ref<OpsSystemLogSinkHealth | null>(null)
const healthLoading = ref(false)
const healthFailed = ref(false)

const runtimeLoading = ref(false)
const runtimeConfigLoaded = ref(false)
const runtimeSaving = ref(false)
const runtimeConfig = reactive<OpsRuntimeLogConfig>({
  level: 'info',
  persist_access_logs: false,
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
  request_retention_days: 90
})

const initialFilters = () => ({
  time_range: DEFAULT_OPS_TIME_RANGE as NonNullable<OpsSystemLogQuery['time_range']>,
  start_time: '',
  end_time: '',
  host: '',
  level: '',
  component: '',
  request_id: '',
  client_request_id: '',
  user_id: '',
  api_key_id: '',
  account_id: '',
  platform: props.platformFilter,
  model: '',
  q: ''
})
const filters = reactive(initialFilters())
const appliedFilters = ref({ ...filters })
const canCleanup = computed(() => !loading.value && !cleaning.value && cleanupScope.value !== null &&
  total.value > 0 && JSON.stringify(filters) === JSON.stringify(appliedFilters.value))

const runtimeLevelOptions = [
  { value: 'debug', label: 'debug' },
  { value: 'info', label: 'info' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' }
]

const stacktraceLevelOptions = [
  { value: 'none', label: 'none' },
  { value: 'error', label: 'error' },
  { value: 'fatal', label: 'fatal' }
]

const timeRangeOptions = computed(() => ['5m', '30m', '1h', '6h', '24h', '7d', '30d'].map(value => ({
  value, label: t(`admin.ops.timeRange.${value}`)
})))

const filterLevelOptions = computed(() => [
  { value: '', label: t('admin.ops.systemLogs.all') },
  { value: 'debug', label: 'debug' },
  { value: 'info', label: 'info' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' }
])

const levelBadgeClass = (level: string) => {
  const v = String(level || '').toLowerCase()
  if (v === 'error' || v === 'fatal') return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (v === 'warn' || v === 'warning') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  if (v === 'debug') return 'bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
}

const formatTime = (value: string) => {
  if (!value) return '-'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString()
}

const getExtraString = (extra: Record<string, any> | undefined, key: string) => {
  if (!extra) return ''
  const v = extra[key]
  if (v == null) return ''
  if (typeof v === 'string') return v.trim()
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  return ''
}

const formatSystemLogDetail = (row: OpsSystemLog) => {
  const parts: string[] = []
  const msg = String(row.message || '').trim()
  if (msg) parts.push(msg)

  const extra = row.extra || {}
  const statusCode = getExtraString(extra, 'status_code')
  const latencyMs = getExtraString(extra, 'latency_ms')
  const method = getExtraString(extra, 'method')
  const path = getExtraString(extra, 'path')
  const clientIP = getExtraString(extra, 'client_ip')
  const protocol = getExtraString(extra, 'protocol')

  const accessParts: string[] = []
  if (statusCode) accessParts.push(`status=${statusCode}`)
  if (latencyMs) accessParts.push(`latency_ms=${latencyMs}`)
  if (method) accessParts.push(`method=${method}`)
  if (path) accessParts.push(`path=${path}`)
  if (clientIP) accessParts.push(`ip=${clientIP}`)
  if (protocol) accessParts.push(`proto=${protocol}`)
  if (accessParts.length > 0) parts.push(accessParts.join(' '))

  const upstreamStatus = getExtraString(extra, 'upstream_status')
  const switchCount = getExtraString(extra, 'switch_count')
  if (upstreamStatus) parts.push(t('admin.ops.systemLogs.upstreamStatus', { status: upstreamStatus }))
  if (switchCount) parts.push(t('admin.ops.systemLogs.switchCount', { count: switchCount }))

  const corrParts: string[] = []
  if (row.request_id) corrParts.push(`req=${row.request_id}`)
  if (row.client_request_id) corrParts.push(`client_req=${row.client_request_id}`)
  if (row.user_id != null) corrParts.push(`user=${row.user_id}`)
  if (row.api_key_id != null) corrParts.push(`key=${row.api_key_id}`)
  if (row.account_id != null) corrParts.push(`acc=${row.account_id}`)
  if (row.platform) corrParts.push(`platform=${row.platform}`)
  if (row.model) corrParts.push(`model=${row.model}`)
  if (corrParts.length > 0) parts.push(corrParts.join(' '))

  const errors = getExtraString(extra, 'errors')
  if (errors) parts.push(`errors=${errors}`)
  const err = getExtraString(extra, 'err') || getExtraString(extra, 'error')
  if (err) parts.push(`error=${err}`)

  // 用空格拼接，交给 CSS 自动换行，尽量“填满再换行”。
  return parts.join('  ')
}

const toRFC3339 = (value: string) => {
  if (!value) return undefined
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) throw new Error(t('admin.ops.systemLogs.invalidFilterTime'))
  return d.toISOString()
}

const filterID = (raw: string, field: string) => {
  const value = raw.trim()
  if (!value) return undefined
  const id = Number(value)
  if (!/^\d+$/.test(value) || !Number.isSafeInteger(id) || id <= 0) {
    throw new Error(t('admin.ops.systemLogs.invalidFilterId', { field }))
  }
  return id
}

const buildFilterQuery = (selected: typeof filters): OpsSystemLogQuery => ({
  time_range: selected.time_range,
  start_time: toRFC3339(selected.start_time),
  end_time: toRFC3339(selected.end_time),
  host: selected.host.trim() || undefined,
  level: selected.level.trim() || undefined,
  component: selected.component.trim() || undefined,
  request_id: selected.request_id.trim() || undefined,
  client_request_id: selected.client_request_id.trim() || undefined,
  user_id: filterID(selected.user_id, 'user_id'),
  api_key_id: filterID(selected.api_key_id, t('admin.ops.systemLogs.keyId')),
  account_id: filterID(selected.account_id, 'account_id'),
  platform: selected.platform.trim() || undefined,
  model: selected.model.trim() || undefined,
  q: selected.q.trim() || undefined
})

const fetchLogs = async () => {
  const request = ++listRequest
  loading.value = true
  listFailed.value = false
  cleanupScope.value = null
  try {
    const { time_range, ...conditions } = buildFilterQuery(appliedFilters.value)
    const res = await opsAPI.listSystemLogs({ ...conditions, time_range, page: page.value, page_size: pageSize.value })
    if (request !== listRequest) return
    logs.value = res.items
    total.value = res.total
    cleanupScope.value = { ...conditions, start_time: res.start_time, end_time: res.end_time }
  } catch (err) {
    if (request !== listRequest) return
    logs.value = []
    total.value = 0
    listFailed.value = true
    console.error('[OpsSystemLogTable] Failed to fetch logs', err)
    appStore.showError(extractApiErrorMessage(err, t('admin.ops.systemLogs.loadFailed')))
  } finally {
    if (request === listRequest) loading.value = false
  }
}

const fetchHealth = async () => {
  if (healthLoading.value) return
  healthLoading.value = true
  healthFailed.value = false
  try {
    health.value = await opsAPI.getSystemLogSinkHealth()
  } catch {
    health.value = null
    healthFailed.value = true
  } finally {
    healthLoading.value = false
  }
}

const loadRuntimeConfig = async () => {
  runtimeLoading.value = true
  runtimeConfigLoaded.value = false
  try {
    const cfg = await opsAPI.getRuntimeLogConfig()
    runtimeConfig.level = cfg.level
    runtimeConfig.persist_access_logs = cfg.persist_access_logs
    runtimeConfig.enable_sampling = cfg.enable_sampling
    runtimeConfig.sampling_initial = cfg.sampling_initial
    runtimeConfig.sampling_thereafter = cfg.sampling_thereafter
    runtimeConfig.caller = cfg.caller
    runtimeConfig.stacktrace_level = cfg.stacktrace_level
    runtimeConfig.retention_days = cfg.retention_days
    runtimeConfig.request_retention_days = cfg.request_retention_days ?? 90
    runtimeConfigLoaded.value = true
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to load runtime log config', err)
    appStore.showError(t('admin.ops.systemLogs.runtimeConfigLoadFailed'))
  } finally {
    runtimeLoading.value = false
  }
}

const saveRuntimeConfig = async () => {
  if (!runtimeConfigLoaded.value || runtimeLoading.value) return
  if (!Number.isInteger(runtimeConfig.retention_days) || runtimeConfig.retention_days < 1 || runtimeConfig.retention_days > 3650 ||
      !Number.isInteger(runtimeConfig.request_retention_days) || runtimeConfig.request_retention_days < 0 || runtimeConfig.request_retention_days > 3650) {
    appStore.showError(t('admin.ops.systemLogs.retentionDaysInvalid'))
    return
  }
  runtimeSaving.value = true
  try {
    const saved = await opsAPI.updateRuntimeLogConfig({ ...runtimeConfig })
    runtimeConfig.level = saved.level
    runtimeConfig.persist_access_logs = saved.persist_access_logs
    runtimeConfig.enable_sampling = saved.enable_sampling
    runtimeConfig.sampling_initial = saved.sampling_initial
    runtimeConfig.sampling_thereafter = saved.sampling_thereafter
    runtimeConfig.caller = saved.caller
    runtimeConfig.stacktrace_level = saved.stacktrace_level
    runtimeConfig.retention_days = saved.retention_days
    runtimeConfig.request_retention_days = saved.request_retention_days ?? 90
    appStore.showSuccess(t('admin.ops.systemLogs.runtimeConfigActive'))
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to save runtime log config', err)
    appStore.showError(err?.response?.data?.detail || t('admin.ops.systemLogs.runtimeConfigSaveFailed'))
  } finally {
    runtimeSaving.value = false
  }
}

const resetRuntimeConfig = async () => {
  const ok = window.confirm(t('admin.ops.systemLogs.resetRuntimeConfigConfirm'))
  if (!ok) return

  runtimeSaving.value = true
  try {
    const saved = await opsAPI.resetRuntimeLogConfig()
    runtimeConfig.level = saved.level
    runtimeConfig.persist_access_logs = saved.persist_access_logs
    runtimeConfig.enable_sampling = saved.enable_sampling
    runtimeConfig.sampling_initial = saved.sampling_initial
    runtimeConfig.sampling_thereafter = saved.sampling_thereafter
    runtimeConfig.caller = saved.caller
    runtimeConfig.stacktrace_level = saved.stacktrace_level
    runtimeConfig.retention_days = saved.retention_days
    runtimeConfig.request_retention_days = saved.request_retention_days ?? 90
    appStore.showSuccess(t('admin.ops.systemLogs.runtimeConfigReset'))
    await fetchHealth()
  } catch (err: any) {
    console.error('[OpsSystemLogTable] Failed to reset runtime log config', err)
    appStore.showError(err?.response?.data?.detail || t('admin.ops.systemLogs.runtimeConfigResetFailed'))
  } finally {
    runtimeSaving.value = false
  }
}

const cleanupCurrentFilter = async () => {
  const payload = cleanupScope.value
  if (!payload || !canCleanup.value) return
  if (!window.confirm(t('admin.ops.systemLogs.cleanupConfirm', {
    start: formatTime(payload.start_time), end: formatTime(payload.end_time), count: total.value
  }))) return

  cleaning.value = true
  try {
    const res = await opsAPI.cleanupSystemLogs(payload)
    appStore.showSuccess(t('admin.ops.systemLogs.cleanupSuccess', { count: res.deleted || 0 }))
    page.value = 1
    await Promise.all([fetchLogs(), fetchHealth()])
  } catch (err) {
    console.error('[OpsSystemLogTable] Failed to cleanup logs', err)
    appStore.showError(
      extractApiErrorMessage(err, t('admin.ops.systemLogs.cleanupFailed'), {
        OPS_SYSTEM_LOG_CLEANUP_RANGE_REQUIRED: t('admin.ops.systemLogs.cleanupRangeRequired'),
        OPS_SYSTEM_LOG_CLEANUP_INVALID_RANGE: t('admin.ops.systemLogs.cleanupInvalidRange')
      })
    )
  } finally {
    cleaning.value = false
  }
}

const resetFilters = () => {
  Object.assign(filters, initialFilters())
  applyFilters()
}

watch(() => props.platformFilter, (v) => {
  filters.platform = v
  appliedFilters.value = { ...appliedFilters.value, platform: v }
  page.value = 1
  fetchLogs()
})

watch(() => props.refreshToken, () => {
  fetchLogs()
  fetchHealth()
})

const onPageChange = (next: number) => {
  page.value = next
  fetchLogs()
}

const onPageSizeChange = (next: number) => {
  pageSize.value = next
  page.value = 1
  fetchLogs()
}

const applyFilters = () => {
  try {
    buildFilterQuery(filters)
  } catch (err) {
    if (!(err instanceof Error)) throw err
    appStore.showError(err.message)
    return
  }
  appliedFilters.value = { ...filters }
  page.value = 1
  fetchLogs()
}

const hasData = computed(() => logs.value.length > 0)

onMounted(async () => {
  await Promise.all([fetchLogs(), fetchHealth(), loadRuntimeConfig()])
})

onBeforeUnmount(() => { listRequest += 1 })
</script>

<template>
  <section class="rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-dark-700 dark:bg-dark-900/60">
    <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.ops.systemLogs.title') }}</h3>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.description') }}</p>
      </div>
      <p v-if="healthLoading" class="text-xs text-gray-500" role="status">{{ t('common.loading') }}</p>
      <p v-else-if="healthFailed" class="text-xs text-red-600 dark:text-red-400" role="status">{{ t('admin.ops.systemLogs.healthLoadFailed') }}</p>
      <div v-else-if="health" class="flex flex-wrap items-center gap-2 text-xs">
        <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.queue') }} {{ health.queue_depth }}/{{ health.queue_capacity }}</span>
        <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-700 dark:bg-dark-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.written') }} {{ health.written_count }}</span>
        <span class="rounded-md bg-amber-100 px-2 py-1 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300">{{ t('admin.ops.systemLogs.dropped') }} {{ health.dropped_count }}</span>
        <span class="rounded-md bg-red-100 px-2 py-1 text-red-700 dark:bg-red-900/30 dark:text-red-300">{{ t('admin.ops.systemLogs.failed') }} {{ health.write_failed_count }}</span>
      </div>
    </div>

    <div class="mb-4 rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-700 dark:bg-dark-800/70">
      <div class="mb-2 flex items-center justify-between">
        <div class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('admin.ops.systemLogs.runtimeConfig') }}</div>
        <span v-if="runtimeLoading" class="text-xs text-gray-500">{{ t('common.loading') }}</span>
      </div>
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-6">
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.level') }}
          <Select v-model="runtimeConfig.level" class="mt-1" :options="runtimeLevelOptions" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.stacktraceThreshold') }}
          <Select v-model="runtimeConfig.stacktrace_level" class="mt-1" :options="stacktraceLevelOptions" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.samplingInitial') }}
          <input v-model.number="runtimeConfig.sampling_initial" type="number" min="1" class="input mt-1" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.samplingThereafter') }}
          <input v-model.number="runtimeConfig.sampling_thereafter" type="number" min="1" class="input mt-1" />
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.retentionDays') }}
          <LogRetentionSelect v-model="runtimeConfig.retention_days" :label="t('admin.ops.systemLogs.retentionDays')" class="mt-1" />
          <span class="mt-1 block text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.retentionDaysHint') }}</span>
        </label>
        <label class="text-xs text-gray-600 dark:text-gray-300">
          {{ t('admin.ops.systemLogs.requestRetentionDays') }}
          <LogRetentionSelect v-model="runtimeConfig.request_retention_days" :label="t('admin.ops.systemLogs.requestRetentionDays')" allow-forever class="mt-1" />
          <span class="mt-1 block text-[11px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.requestRetentionDaysHint') }}</span>
        </label>
        <div class="md:col-span-2 xl:col-span-6">
          <div class="grid gap-3 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-end">
            <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.caller" type="checkbox" />
                {{ t('admin.ops.systemLogs.caller') }}
              </label>
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.enable_sampling" type="checkbox" />
                {{ t('admin.ops.systemLogs.sampling') }}
              </label>
              <label class="inline-flex items-center gap-2 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="runtimeConfig.persist_access_logs" type="checkbox" />
                {{ t('admin.ops.systemLogs.persistAccessLogs') }}
              </label>
            </div>
            <div class="flex flex-wrap items-center gap-2 lg:justify-end">
              <button type="button" class="btn btn-primary btn-sm" :disabled="runtimeSaving || runtimeLoading || !runtimeConfigLoaded" @click="saveRuntimeConfig">
                {{ runtimeSaving ? t('common.saving') : t('admin.ops.systemLogs.saveAndApply') }}
              </button>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="runtimeSaving" @click="resetRuntimeConfig">
                {{ t('admin.ops.systemLogs.resetDefaults') }}
              </button>
            </div>
          </div>
        </div>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.systemLogs.persistAccessLogsHint') }}</p>
      <p v-if="!healthLoading && health?.last_error" class="mt-2 text-xs text-red-600 dark:text-red-400">{{ t('admin.ops.systemLogs.latestWriteError') }} {{ health.last_error }}</p>
    </div>

    <div class="mb-4 grid grid-cols-1 gap-3 md:grid-cols-5">
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.timeRange') }}
        <Select v-model="filters.time_range" class="mt-1" :options="timeRangeOptions" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.startTime') }}
        <input v-model="filters.start_time" type="datetime-local" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.endTime') }}
        <input v-model="filters.end_time" type="datetime-local" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.level') }}
        <Select v-model="filters.level" class="mt-1" :options="filterLevelOptions" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.component') }}
        <input v-model="filters.component" type="text" class="input mt-1" :placeholder="t('admin.ops.systemLogs.componentPlaceholder')" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.host') }}
        <input v-model="filters.host" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        request_id
        <input v-model="filters.request_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        client_request_id
        <input v-model="filters.client_request_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        user_id
        <input v-model="filters.user_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.keyId') }}
        <input v-model="filters.api_key_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        account_id
        <input v-model="filters.account_id" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.platform') }}
        <input v-model="filters.platform" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.model') }}
        <input v-model="filters.model" type="text" class="input mt-1" />
      </label>
      <label class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.systemLogs.keyword') }}
        <input v-model="filters.q" type="text" class="input mt-1" :placeholder="t('admin.ops.systemLogs.keywordPlaceholder')" />
      </label>
    </div>

    <div class="mb-3 flex flex-wrap gap-2">
      <button type="button" class="btn btn-primary btn-sm" @click="applyFilters">{{ t('admin.ops.systemLogs.search') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" @click="resetFilters">{{ t('common.reset') }}</button>
      <button type="button" class="btn btn-danger btn-sm" :disabled="!canCleanup" @click="cleanupCurrentFilter">{{ t('admin.ops.systemLogs.cleanCurrentFilters') }}</button>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="healthLoading" @click="fetchHealth">{{ t('admin.ops.systemLogs.refreshHealth') }}</button>
    </div>

    <div class="overflow-hidden rounded-xl border border-gray-200 dark:border-dark-700">
      <div v-if="loading" class="px-4 py-8 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
      <div v-else-if="listFailed" class="px-4 py-8 text-center text-sm text-red-600 dark:text-red-400" role="status">{{ t('admin.ops.systemLogs.loadFailed') }}</div>
      <div v-else-if="!hasData" class="px-4 py-8 text-center text-sm text-gray-500">{{ t('admin.ops.systemLogs.empty') }}</div>
      <div v-else-if="!isDesktopViewport" class="divide-y divide-gray-100 dark:divide-dark-800">
        <div v-for="row in logs" :key="row.id" class="space-y-1.5 p-3">
          <div class="flex items-center justify-between gap-2">
            <span class="inline-flex rounded-full px-2 py-0.5 text-xs font-semibold" :class="levelBadgeClass(row.level)">
              {{ row.level }}
            </span>
            <span class="text-xs text-gray-500 dark:text-gray-400">{{ formatTime(row.created_at) }}</span>
          </div>
          <div v-if="row.host" class="truncate text-xs text-gray-500 dark:text-gray-400" :title="row.host">
            {{ row.host }}
          </div>
          <div class="whitespace-normal break-all text-xs text-gray-700 dark:text-gray-300">
            {{ formatSystemLogDetail(row) }}
          </div>
        </div>
      </div>
      <div v-else class="overflow-auto">
        <table class="min-w-full table-fixed divide-y divide-gray-200 dark:divide-dark-700">
          <thead class="bg-gray-50 dark:bg-dark-900">
            <tr>
              <th class="w-[170px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.time') }}</th>
              <th class="w-[160px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.host') }}</th>
              <th class="w-[80px] px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.level') }}</th>
              <th class="px-3 py-2 text-left text-[11px] font-semibold text-gray-500">{{ t('admin.ops.systemLogs.logDetails') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-800">
            <tr v-for="row in logs" :key="row.id" class="align-top">
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300">{{ formatTime(row.created_at) }}</td>
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300">
                <span class="block truncate" :title="row.host || '-'">{{ row.host || '-' }}</span>
              </td>
              <td class="px-3 py-2 text-xs">
                <span class="inline-flex rounded-full px-2 py-0.5 font-semibold" :class="levelBadgeClass(row.level)">
                  {{ row.level }}
                </span>
              </td>
              <td class="px-3 py-2 text-xs text-gray-700 dark:text-gray-300 whitespace-normal break-all">
                {{ formatSystemLogDetail(row) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <Pagination
        :total="total"
        :page="page"
        :page-size="pageSize"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
      />
    </div>
  </section>
</template>
