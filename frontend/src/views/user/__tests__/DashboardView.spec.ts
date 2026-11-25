import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import Dashboard from '../DashboardView.vue'

const api = vi.hoisted(() => ({ getDashboardStats: vi.fn(), getDashboardTrend: vi.fn(), getDashboardModels: vi.fn(), getByDateRange: vi.fn() }))
vi.mock('@/api/usage', () => ({ usageAPI: api }))
vi.mock('@/api/user', () => ({ getMyPlatformQuotas: async () => ({ platform_quotas: [] }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ refreshUser: async () => {}, user: { balance: 10 }, isSimpleMode: false }) }))
vi.mock('vue-i18n', async (original) => ({ ...await original<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))
const mountPage = () => shallowMount(Dashboard, { global: { stubs: { AppLayout: { template: '<main><slot/></main>' } } } })
beforeEach(() => {
  api.getDashboardStats.mockResolvedValue({})
  api.getDashboardTrend.mockResolvedValue({ trend: [] })
  api.getDashboardModels.mockResolvedValue({ models: [] })
  api.getByDateRange.mockResolvedValue({ items: [] })
})

it('preserves the current chart range after a slower request completes', async () => {
  let finishOld!: (value: unknown) => void
  api.getDashboardTrend.mockImplementationOnce(() => new Promise(resolve => { finishOld = resolve }))
  const page = mountPage()
  await flushPromises()
  const charts = page.findComponent({ name: 'UserDashboardCharts' })
  api.getDashboardTrend.mockResolvedValueOnce({ trend: [{ date: 'current' }] })
  charts.vm.$emit('update:startDate', '2026-01-01')
  charts.vm.$emit('dateRangeChange')
  await flushPromises()
  finishOld({ trend: [{ date: 'old' }] })
  await flushPromises()
  expect(charts.props('trend')).toEqual([{ date: 'current' }])
  page.unmount()
})

it('shows independent panels and retries a failed initial stats request', async () => {
  api.getDashboardStats.mockRejectedValueOnce(new Error('unavailable'))
  const page = mountPage()
  await flushPromises()
  expect(page.get('[role="alert"]').text()).toContain('dashboard.statsLoadFailed')
  expect(page.findComponent({ name: 'UserDashboardRecentUsage' }).exists()).toBe(true)
  await page.get('[role="alert"] button').trigger('click')
  await flushPromises()
  expect(page.find('[role="alert"]').exists()).toBe(false)
  expect(page.findComponent({ name: 'UserDashboardStats' }).exists()).toBe(true)
  page.unmount()
})
