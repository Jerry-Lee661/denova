/**
 * Shared formatting utilities.
 *
 * `formatDuration` was originally a private function inside AgentTracePanel.
 * It is now extracted here so that other panels (e.g. ImageAnalysisPanel)
 * can reuse the same human-readable duration format.
 */

/**
 * Format a duration in milliseconds to a compact human-readable string.
 *
 * Examples:  800 → "800ms",  5100 → "5.1s",  62000 → "62s"
 */
export function formatDuration(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '-'
  if (value < 1000) return `${Math.round(value)}ms`
  return `${(value / 1000).toFixed(value < 10000 ? 1 : 0)}s`
}

/**
 * Format a token count to a compact human-readable string (Copilot-style stats).
 *
 * Examples: 1234 → "1234",  12345 → "12.3k",  123456 → "123.5k"
 */
export function formatTokenCount(value?: number): string {
  if (!Number.isFinite(value) || (value ?? 0) <= 0) return ''
  if (value! >= 10000) return `${(value! / 1000).toFixed(1)}k`
  return String(value)
}

/**
 * Format a generation speed (tokens/second) to a compact string.
 *
 * Examples: 8.24 → "8.2 tok/s", 12.0 → "12 tok/s"
 */
export function formatTokensPerSecond(value?: number): string {
  if (!Number.isFinite(value) || (value ?? 0) <= 0) return ''
  return `${value!.toFixed(value! < 10 ? 1 : 0)} tok/s`
}
