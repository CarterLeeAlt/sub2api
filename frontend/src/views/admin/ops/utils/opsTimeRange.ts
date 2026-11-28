import { parseTimeRangeMinutes } from './opsFormatters'

export const DEFAULT_OPS_TIME_RANGE = '24h'

export function buildOpsTimeParams(
  timeRange: string,
  customStartTime?: string | null,
  customEndTime?: string | null,
  anchor?: Date
): Record<string, string> {
  if (timeRange === 'custom' && customStartTime && customEndTime) {
    return { start_time: customStartTime, end_time: customEndTime }
  }

  if (anchor) {
    const minutes = parseTimeRangeMinutes(timeRange === 'custom' ? DEFAULT_OPS_TIME_RANGE : timeRange)
    return { start_time: new Date(anchor.getTime() - minutes * 60_000).toISOString(), end_time: anchor.toISOString() }
  }

  return { time_range: timeRange === 'custom' ? DEFAULT_OPS_TIME_RANGE : timeRange }
}
