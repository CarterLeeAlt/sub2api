import type { Account } from '@/types'

export type ModelCatalogSource = 'upstream' | 'builtin' | 'custom'
export interface ModelCatalogState {
  source?: ModelCatalogSource
  revision?: string
  ready?: boolean
  synced_at?: string
  attempted_at?: string
  error?: string
  added?: number
  removed?: number
}

export function supportsAutomaticModelCatalog(account: Pick<Account, 'platform' | 'type' | 'credentials' | 'extra' | 'parent_account_id'>): boolean {
  const authMode = String(account.credentials?.auth_mode ?? '').trim().toLowerCase()
  const legacyAuthMode = String(account.credentials?.openai_auth_mode ?? '').trim().toLowerCase()
  const isPAT = (mode: string) => ['personalaccesstoken', 'personal_access_token'].includes(mode)
  return account.platform === 'openai' && account.type === 'oauth' && !account.parent_account_id &&
    authMode !== 'agentidentity' && !isPAT(authMode) && !isPAT(legacyAuthMode) &&
    account.extra?.openai_passthrough !== true && account.extra?.openai_oauth_passthrough !== true
}

export function accountModelCatalog(account: Pick<Account, 'extra'> | null): ModelCatalogState {
  return (account?.extra?.model_catalog as ModelCatalogState | undefined) ?? {}
}
