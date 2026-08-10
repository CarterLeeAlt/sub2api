const fullGitCommitPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/i
const shortCommitLength = 7

export function formatBuildIdentity(commit?: string | null): string {
  const normalized = commit?.trim().toLowerCase() || ''
  if (!fullGitCommitPattern.test(normalized)) return ''

  return `sha-${normalized.slice(0, shortCommitLength)}`
}
