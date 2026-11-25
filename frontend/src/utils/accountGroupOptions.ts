import type { GroupPlatform } from '@/types'

export function accountGroupOptions<T extends { platform: GroupPlatform }>(
  groups: T[], platform: GroupPlatform | undefined, mixedScheduling: boolean, simpleMode: boolean,
): T[] {
  return groups.filter(group => {
    if (group.platform === 'composite') return !simpleMode
    if (!platform || platform === group.platform) return true
    return platform === 'antigravity' && mixedScheduling && (group.platform === 'anthropic' || group.platform === 'gemini')
  })
}
