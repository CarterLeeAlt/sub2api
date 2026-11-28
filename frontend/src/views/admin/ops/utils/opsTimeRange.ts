export const DEFAULT_OPS_TIME_RANGE = '24h'

export function buildOpsTimeParams(
  timeRange: string,
  customStartTime?: string | null,
  customEndTime?: string | null
): Record<string, string> {
  if (timeRange === 'custom' && customStartTime && customEndTime) {
    return { start_time: customStartTime, end_time: customEndTime }
  }

  return { time_range: timeRange === 'custom' ? DEFAULT_OPS_TIME_RANGE : timeRange }
}
