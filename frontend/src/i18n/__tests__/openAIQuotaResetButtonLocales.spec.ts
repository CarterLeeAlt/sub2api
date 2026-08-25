import { describe, expect, it } from 'vitest'
import enAdminAccounts from '../locales/en/admin/accounts'
import zhAdminAccounts from '../locales/zh/admin/accounts'

describe('OpenAI quota reset button labels', () => {
  it('keeps the existing Chinese action names unchanged', () => {
    expect(zhAdminAccounts.accounts.usageWindow.activeQuery).toBe('查询')
    expect(zhAdminAccounts.accounts.openaiQuotaReset.count).toBe('次数')
    expect(zhAdminAccounts.accounts.openaiQuotaReset.reset).toBe('重置')
  })

  it('keeps the existing English action names unchanged', () => {
    expect(enAdminAccounts.accounts.usageWindow.activeQuery).toBe('Query')
    expect(enAdminAccounts.accounts.openaiQuotaReset.count).toBe('Credits')
    expect(enAdminAccounts.accounts.openaiQuotaReset.reset).toBe('Reset')
  })
})
