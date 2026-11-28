import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'
import LogRetentionSelect from '../LogRetentionSelect.vue'
import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'

const mockListSystemLogs = vi.fn()
const mockCleanupSystemLogs = vi.fn()
const mockGetSystemLogSinkHealth = vi.fn()
const mockGetRuntimeLogConfig = vi.fn()
const mockUpdateRuntimeLogConfig = vi.fn()
const mockShowError = vi.fn()

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    listSystemLogs: (...args: any[]) => mockListSystemLogs(...args),
    cleanupSystemLogs: (...args: any[]) => mockCleanupSystemLogs(...args),
    getSystemLogSinkHealth: (...args: any[]) => mockGetSystemLogSinkHealth(...args),
    getRuntimeLogConfig: (...args: any[]) => mockGetRuntimeLogConfig(...args),
    updateRuntimeLogConfig: (...args: any[]) => mockUpdateRuntimeLogConfig(...args),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: (...args: any[]) => mockShowError(...args),
    showSuccess: vi.fn(),
  }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const SelectStub = defineComponent({
  name: 'SelectControlStub',
  props: {
    modelValue: {
      type: [String, Number],
      default: '',
    },
  },
  emits: ['update:modelValue'],
  template: '<div class="select-stub" />',
})

const PaginationStub = defineComponent({
  name: 'PaginationStub',
  template: '<div class="pagination-stub" />',
})

const runtimeConfig = {
  level: 'info',
  persist_access_logs: false,
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
  request_retention_days: 90,
}

const sinkHealth = {
  queue_depth: 0,
  queue_capacity: 5000,
  dropped_count: 0,
  write_failed_count: 0,
  written_count: 1,
  avg_write_delay_ms: 0,
}

const queryBounds = { start_time: '2026-11-28T04:00:00.123Z', end_time: '2026-11-28T05:00:00.456Z' }

describe('OpsSystemLogTable host support', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListSystemLogs.mockResolvedValue({
      items: [
        {
          id: 1,
          created_at: '2026-07-14T00:10:01Z',
          host: 'api-node-1',
          level: 'warn',
          component: 'app',
          message: 'request failed',
        },
      ],
      total: 1,
      page: 1,
      page_size: 20,
      ...queryBounds,
    })
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
  })

  it('renders the host and sends it with list and cleanup filters', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('api-node-1')

    const hostLabel = wrapper.findAll('label').find((label) => label.text().includes('admin.ops.systemLogs.host'))
    expect(hostLabel).toBeDefined()
    await hostLabel!.find('input').setValue(' api-node-2 ')

    const searchButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.search')
    expect(searchButton).toBeDefined()
    await searchButton!.trigger('click')
    await flushPromises()

    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ host: 'api-node-2' }))

    const cleanupButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.cleanCurrentFilters')
    expect(cleanupButton).toBeDefined()
    await cleanupButton!.trigger('click')
    await flushPromises()

    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ host: 'api-node-2', ...queryBounds }))
    expect(mockCleanupSystemLogs.mock.calls[0]?.[0]).not.toHaveProperty('time_range')
  })

  it('keeps database access-log persistence opt-in', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    const label = wrapper.findAll('label').find((item) => item.text().includes('admin.ops.systemLogs.persistAccessLogs'))
    expect(label).toBeDefined()
    expect((label!.find('input').element as HTMLInputElement).checked).toBe(false)
  })

  it.each([
    ['zh', zhLocale],
    ['en', enLocale],
  ])('defines the Host translation for %s', (_name, locale) => {
    expect(locale.admin.ops.systemLogs.host).toBe('Host')
  })
})

describe('system log search and health', () => {
  const mountTable = (platformFilter = '') => mount(OpsSystemLogTable, {
    props: { platformFilter },
    global: { stubs: { Select: SelectStub, Pagination: PaginationStub } },
  })
  const button = (wrapper: ReturnType<typeof mountTable>, key: string) =>
    wrapper.findAll('button').find(item => item.text() === `admin.ops.systemLogs.${key}`)!
  const input = (wrapper: ReturnType<typeof mountTable>, key: string) =>
    wrapper.findAll('label').find(item => item.text() === `admin.ops.systemLogs.${key}`)!.find('input')
  const result = (message = 'request failed', extra?: Record<string, unknown>) => ({
    items: [{ id: 1, created_at: queryBounds.start_time, level: 'warn', message, extra }],
    total: 1, page: 1, page_size: 20, ...queryBounds,
  })

  beforeEach(() => {
    vi.clearAllMocks()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListSystemLogs.mockResolvedValue(result())
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
  })

  it('requires searching edited filters before cleanup and honors cancel and confirm', async () => {
    const wrapper = mountTable()
    await flushPromises()

    const cleanup = button(wrapper, 'cleanCurrentFilters')
    await input(wrapper, 'keyword').setValue('timeout')
    expect(cleanup.attributes('disabled')).toBeDefined()
    await wrapper.setProps({ refreshToken: 1 })
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall?.[0].q).toBeUndefined()
    expect(mockCleanupSystemLogs).not.toHaveBeenCalled()

    await button(wrapper, 'search').trigger('click')
    await flushPromises()
    expect(cleanup.attributes('disabled')).toBeUndefined()
    vi.mocked(window.confirm).mockReturnValueOnce(false)
    await cleanup.trigger('click')
    expect(mockCleanupSystemLogs).not.toHaveBeenCalled()
    await cleanup.trigger('click')
    await flushPromises()
    expect(mockCleanupSystemLogs).toHaveBeenCalledOnce()
    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ q: 'timeout', ...queryBounds }))
    wrapper.unmount()
  })

  it('updates the search when the top platform changes or is cleared', async () => {
    const wrapper = mountTable('openai')
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall?.[0].platform).toBe('openai')
    await wrapper.setProps({ platformFilter: 'anthropic' })
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall?.[0].platform).toBe('anthropic')
    await wrapper.setProps({ platformFilter: '' })
    await flushPromises()
    expect(mockListSystemLogs.mock.lastCall?.[0].platform).toBeUndefined()
    expect((input(wrapper, 'platform').element as HTMLInputElement).value).toBe('')
    wrapper.unmount()
  })

  it('ignores a slower previous search and blocks cleanup after a failed query', async () => {
    const wrapper = mountTable()
    await flushPromises()
    let finishOld: (response: ReturnType<typeof result>) => void = () => {}
    mockListSystemLogs.mockImplementationOnce(() => new Promise(resolve => { finishOld = resolve }))
    await input(wrapper, 'keyword').setValue('old')
    await button(wrapper, 'search').trigger('click')
    expect(button(wrapper, 'cleanCurrentFilters').attributes('disabled')).toBeDefined()
    mockListSystemLogs.mockResolvedValueOnce(result('latest entry'))
    await input(wrapper, 'keyword').setValue('latest')
    await button(wrapper, 'search').trigger('click')
    await flushPromises()
    finishOld(result('obsolete entry'))
    await flushPromises()
    expect(wrapper.text()).toContain('latest entry')
    expect(wrapper.text()).not.toContain('obsolete entry')

    mockListSystemLogs.mockRejectedValueOnce(new Error('query failed'))
    await button(wrapper, 'search').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.systemLogs.loadFailed')
    expect(wrapper.text()).not.toContain('latest entry')
    expect(button(wrapper, 'cleanCurrentFilters').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('separates unavailable health from measured zeroes and recovers on refresh', async () => {
    mockGetSystemLogSinkHealth.mockRejectedValueOnce(new Error('not available'))
    const wrapper = mountTable()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.systemLogs.healthLoadFailed')
    expect(wrapper.text()).not.toContain('admin.ops.systemLogs.failed')

    await button(wrapper, 'refreshHealth').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.systemLogs.failed 0')
    expect(wrapper.text()).not.toContain('admin.ops.systemLogs.healthLoadFailed')

    mockGetSystemLogSinkHealth.mockRejectedValueOnce(new Error('lost connection'))
    await button(wrapper, 'refreshHealth').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.systemLogs.healthLoadFailed')
    expect(wrapper.text()).not.toContain('admin.ops.systemLogs.queue')
    wrapper.unmount()
  })

  it('renders upstream status and switch count when the log has these fields', async () => {
    mockListSystemLogs.mockResolvedValueOnce(result('switching account', { upstream_status: 503, switch_count: 0 }))
    const wrapper = mountTable()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.ops.systemLogs.upstreamStatus')
    expect(wrapper.text()).toContain('admin.ops.systemLogs.switchCount')
    wrapper.unmount()
  })
})

describe('rolling log retention settings', () => {
  const mountTable = () => mount(OpsSystemLogTable, {
    global: { stubs: { Select: SelectStub, Pagination: PaginationStub } }
  })
  const saveButton = (wrapper: ReturnType<typeof mountTable>) => wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.saveAndApply')!

  beforeEach(() => {
    vi.clearAllMocks()
    mockListSystemLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
  })

  it('loads and saves separate operations and request retention windows', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const fields = wrapper.findAllComponents(LogRetentionSelect)
    expect(fields.map(field => field.props('modelValue'))).toEqual([30, 90])
    fields[0]!.vm.$emit('update:modelValue', 7)
    fields[1]!.vm.$emit('update:modelValue', 180)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ retention_days: 7, request_retention_days: 180 }))
    wrapper.unmount()
  })

  it('supports forever and rejects fractional or empty custom days', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const requestField = wrapper.findAllComponents(LogRetentionSelect)[1]!
    requestField.vm.$emit('update:modelValue', 0)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ request_retention_days: 0 }))
    mockUpdateRuntimeLogConfig.mockClear()
    for (const value of [1.5, Number.NaN, -1, 3651]) {
      requestField.vm.$emit('update:modelValue', value)
      await saveButton(wrapper).trigger('click')
    }
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    expect(mockShowError).toHaveBeenCalledWith('admin.ops.systemLogs.retentionDaysInvalid')
    wrapper.unmount()
  })

  it('does not overwrite saved retention with defaults after a load failure', async () => {
    mockGetRuntimeLogConfig.mockRejectedValue(new Error('unavailable'))
    const wrapper = mountTable()
    await flushPromises()
    expect(saveButton(wrapper).attributes('disabled')).toBeDefined()
    await saveButton(wrapper).trigger('click')
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
