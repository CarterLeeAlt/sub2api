import { describe, expect, it } from 'vitest'
import { buildOpsTimeParams } from '../opsTimeRange'

describe('buildOpsTimeParams', () => {
  it('uses explicit timestamps for a complete custom range', () => {
    expect(buildOpsTimeParams('custom', '2026-08-01T00:00:00Z', '2026-08-02T00:00:00Z')).toEqual({
      start_time: '2026-08-01T00:00:00Z',
      end_time: '2026-08-02T00:00:00Z'
    })
  })

  it('preserves predefined ranges and falls back for incomplete custom ranges', () => {
    expect(buildOpsTimeParams('1h')).toEqual({ time_range: '1h' })
    expect(buildOpsTimeParams('custom', null, null)).toEqual({ time_range: '24h' })
  })
})
