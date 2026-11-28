import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsDashboardHeader from '../OpsDashboardHeader.vue'
import type { OpsDashboardOverview } from '@/api/admin/ops'

vi.mock('@/api', () => ({ adminAPI: { groups: { getAll: vi.fn().mockResolvedValue([]) } } }))
vi.mock('@/api/admin/ops', () => ({ opsAPI: { getRealtimeTrafficSummary: vi.fn().mockResolvedValue({ enabled: true, summary: null }) } }))
vi.mock('@/stores', () => ({ useAdminSettingsStore: () => ({ opsRealtimeMonitoringEnabled: true, setOpsRealtimeMonitoringEnabledLocal: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params ? `${key}:${JSON.stringify(params)}` : key })
}))

const overview = {
  start_time: '2026-11-28T08:00:00Z', end_time: '2026-11-28T08:01:00Z', tps: { avg: 242.7 }
} as OpsDashboardOverview
function withSpeed(speed: OpsDashboardOverview['output_tps']): OpsDashboardOverview {
  if (!speed) return overview
  const rates = { current: 0, avg: 0, peak: 0 }
  return { ...overview, delivery: { seconds: 60, requests: 0, unit: 'char', speed_unit: 'token', rate: rates, qps: rates, speed, ttft: {}, output: 0 } as NonNullable<OpsDashboardOverview['delivery']> }
}
function mountHeader(value = overview) {
  return mount(OpsDashboardHeader, {
    props: { overview: value, platform: '', groupId: null, timeRange: '1h', queryMode: 'auto', loading: false, lastUpdated: null },
    global: { stubs: { Select: true, HelpTooltip: true, BaseDialog: true, Icon: true } }
  })
}

describe('OpsDashboardHeader output TPS', () => {

  it('shows per-request P50, slower-tail percentiles and sample count separately from traffic TPS', async () => {
    const wrapper = mountHeader(withSpeed({ p5: 2.35, p10: 3.7, p50: 55, avg: 277.75, sample_count: 4 }))
    await flushPromises()
    const card = wrapper.get('[data-testid="output-tps-card"]')
    expect(card.get('[data-testid="output-tps-p50"]').text()).toBe('55.0')
    expect(card.get('[data-testid="output-tps-p5"]').text()).toBe('2.4')
    expect(card.get('[data-testid="output-tps-p10"]').text()).toBe('3.7')
    expect(card.get('[data-testid="output-tps-avg"]').text()).toBe('277.8')
    expect(card.get('[data-testid="output-tps-samples"]').text()).toContain('4')
    expect(card.text()).toContain('tok/s (P50)')
    expect(card.text()).not.toContain('242.7')
    await wrapper.setProps({ fullscreen: true, overview: withSpeed({ p5: 0.01, p10: 0.02, p50: 0.04, avg: 0.05, sample_count: 1 }) })
    expect(card.get('[data-testid="output-tps-p50"]').text()).toBe('0.0')
    expect(card.get('[data-testid="output-tps-samples"]').text()).toContain('1')
    wrapper.unmount()
  })

  it.each([undefined, null, { p5: null, p10: null, p50: null, avg: null, sample_count: 0 }])('shows missing output TPS as unavailable, not zero: %s', async (output_tps) => {
    const wrapper = mountHeader(withSpeed(output_tps))
    await flushPromises()
    for (const metric of ['p5', 'p10', 'p50', 'avg']) {
      expect(wrapper.get(`[data-testid="output-tps-${metric}"]`).text()).toBe('—')
    }
    expect(wrapper.get('[data-testid="output-tps-samples"]').text()).toContain(output_tps == null ? '—' : '0')
    wrapper.unmount()
  })

})

describe('OpsDashboardHeader historical coverage', () => {
  it('uses full-window SLA and request totals with partial delivery data', async () => {
    const wrapper = mountHeader({
      ...withSpeed({ p50: 55, sample_count: 4 }),
      start_time: '2026-11-27T08:01:00Z',
      request_count_total: 4034,
      request_count_sla: 3866,
      error_count_sla: 141,
      business_limited_count: 168,
      sla: 3725 / 3866,
      error_rate: 141 / 3866,
    })
    const sla = wrapper.get('[data-testid="ops-sla-card"]')
    expect(sla.text()).toContain('96.353%')
    expect(sla.text()).toContain('141')
    expect(wrapper.get('[data-testid="ops-request-errors-card"]').text()).toContain('3.65%')
    expect(wrapper.get('[data-testid="output-tps-p50"]').text()).toBe('—')
    await sla.get('button').trigger('click')
    expect(wrapper.emitted('openErrorDetails')).toEqual([['request']])
    wrapper.unmount()
  })
})
