import { describe, expect, it } from 'vitest'

import { formatBuildIdentity } from '../buildIdentity'

describe('build identity formatting', () => {
  it('matches the short SHA Docker image tag', () => {
    expect(formatBuildIdentity('57a1f29fc6c71168d6a3a092b4a4611c4e3c58ad')).toBe('sha-57a1f29')
  })

  it('normalizes whitespace and hex casing', () => {
    expect(formatBuildIdentity('  57A1F29FC6C71168D6A3A092B4A4611C4E3C58AD  ')).toBe('sha-57a1f29')
  })

  it.each([undefined, null, '', 'docker', 'unknown', '57a1f29', 'not-a-git-commit'])(
    'hides invalid build commit %s',
    commit => {
      expect(formatBuildIdentity(commit)).toBe('')
    }
  )
})
