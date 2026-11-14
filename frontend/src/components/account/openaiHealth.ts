import type { Account } from '@/types'

export interface OpenAIHealth {
  kind: string
  message: string
  source?: string
  observed_at?: string
}

export function openAIHealth(account: Account): OpenAIHealth | null {
  const health = account.extra?.openai_upstream_health as OpenAIHealth | undefined
  if (health?.kind) return health
  if (account.platform === 'openai' && account.type === 'oauth' && account.status === 'error') {
    return { kind: 'account_error', message: account.error_message || '' }
  }
  return null
}

export function openAIErrorKind(error: unknown): string {
  const value = error as { code?: string; reason?: string }
  const code = typeof value?.code === 'string' ? value.code : value?.reason
  return code?.startsWith('OPENAI_UPSTREAM_') ? code.slice('OPENAI_UPSTREAM_'.length).toLowerCase() : 'request_failed'
}

export function openAIHealthLabel(kind?: string): string {
  return `admin.accounts.openaiHealth.${['auth_invalid', 'auth_refresh_required', 'permission_denied', 'quota_exhausted', 'rate_limited', 'network_error', 'account_error'].includes(kind || '') ? kind : 'request_failed'}`
}
