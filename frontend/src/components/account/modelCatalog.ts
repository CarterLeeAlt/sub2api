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
  return account.platform === 'openai' && account.type === 'oauth' && !account.parent_account_id &&
    account.extra?.openai_passthrough !== true && account.extra?.openai_oauth_passthrough !== true
}

export function accountModelCatalog(account: Pick<Account, 'extra'> | null): ModelCatalogState {
  return (account?.extra?.model_catalog as ModelCatalogState | undefined) ?? {}
}
