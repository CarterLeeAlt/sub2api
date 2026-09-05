import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpenAIQuotaResetCell from '../OpenAIQuotaResetCell.vue'
import type { Account } from '@/types'
import { refreshOpenAIQuota, resetOpenAIQuota } from '@/api/admin/accounts'

// 开关状态通过 hoisted 对象在用例间切换，验证 PAT 卡片在开关开/关下的交互差异。
const patFlag = vi.hoisted(() => ({ enabled: false }))

vi.mock('@/api/admin/accounts', () => ({
  refreshOpenAIQuota: vi.fn(),
  resetOpenAIQuota: vi.fn(),
}))

vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({
    openaiCodexPATResetCreditsEnabled: patFlag.enabled,
  }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params?.time ? `${key}:${params.time}` : params?.count ? `${key}:${params.count}` : key,
    }),
  }
})

const PAT_ACCOUNT: Account = {
  id: 900,
  platform: 'openai',
  type: 'oauth',
  credentials: { auth_mode: 'personalAccessToken' },
} as unknown as Account

const OAUTH_ACCOUNT: Account = {
  id: 901,
  platform: 'openai',
  type: 'oauth',
  credentials: {},
} as unknown as Account

const mountCell = (account: Account) =>
  mount(OpenAIQuotaResetCell, {
    props: { account },
    global: {
      stubs: { ConfirmDialog: { template: '<div />' } },
    },
  })

const findButtons = (wrapper: ReturnType<typeof mountCell>) => {
  const buttons = wrapper.findAll('button')
  return { count: buttons[0], reset: buttons[1] }
}

beforeEach(() => {
  patFlag.enabled = false
  vi.mocked(refreshOpenAIQuota).mockReset()
  vi.mocked(resetOpenAIQuota).mockReset()
})

describe('OpenAIQuotaResetCell — Codex PAT 禁用态', () => {
  it('PAT + 开关关闭：次数可查询，重置禁用且不触发请求，无额外提示行', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      fetched_at: Math.floor(Date.now() / 1000),
      rate_limit_reset_credits: { available_count: 3 },
    } as Awaited<ReturnType<typeof refreshOpenAIQuota>>)

    const wrapper = mountCell(PAT_ACCOUNT)
    expect(wrapper.find('[data-testid="reset-credit-pat-blocked"]').exists()).toBe(false)

    const { count, reset } = findButtons(wrapper)
    expect(count.attributes('disabled')).toBeUndefined()
    expect(reset.attributes('disabled')).toBeDefined()

    await count.trigger('click')
    await flushPromises()
    expect(refreshOpenAIQuota).toHaveBeenCalledWith(PAT_ACCOUNT.id)

    await reset.trigger('click')
    await flushPromises()
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
  })

  it('PAT + 开关开启：恢复可点击，查询正常发起', async () => {
    patFlag.enabled = true
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      fetched_at: Math.floor(Date.now() / 1000),
      rate_limit_reset_credits: { available_count: 2 },
    } as Awaited<ReturnType<typeof refreshOpenAIQuota>>)

    const wrapper = mountCell(PAT_ACCOUNT)
    expect(wrapper.find('[data-testid="reset-credit-pat-blocked"]').exists()).toBe(false)

    const { count } = findButtons(wrapper)
    expect(count.attributes('disabled')).toBeUndefined()

    await count.trigger('click')
    await flushPromises()
    expect(refreshOpenAIQuota).toHaveBeenCalledWith(PAT_ACCOUNT.id)
  })

  it('普通 OAuth 账号不受 PAT 开关影响：开关关闭时仍可查询', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      fetched_at: Math.floor(Date.now() / 1000),
      rate_limit_reset_credits: { available_count: 1 },
    } as Awaited<ReturnType<typeof refreshOpenAIQuota>>)

    const wrapper = mountCell(OAUTH_ACCOUNT)
    expect(wrapper.find('[data-testid="reset-credit-pat-blocked"]').exists()).toBe(false)

    const { count } = findButtons(wrapper)
    expect(count.attributes('disabled')).toBeUndefined()

    await count.trigger('click')
    await flushPromises()
    expect(refreshOpenAIQuota).toHaveBeenCalledWith(OAUTH_ACCOUNT.id)
  })
})
