import { useEffect, useRef, useState } from 'react'

/**
 * Real-time elapsed timer hook.
 *
 * Given an ISO-8601 start timestamp and a `running` flag, returns the
 * elapsed milliseconds since `startedAt`.  While `running` is true the
 * value ticks every second; once `running` becomes false the hook
 * freezes at the final value (or uses `finishedAt` if provided).
 *
 * This is intentionally simple — a single setInterval at 1 s cadence is
 * more than enough for a task-level stopwatch and avoids the overhead of
 * requestAnimationFrame.
 */
export function useElapsed(
  startedAt?: string | null,
  running?: boolean,
  finishedAt?: string | null,
): number {
  const [elapsed, setElapsed] = useState(0)
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null)

  useEffect(() => {
    // Clear any previous interval.
    if (intervalRef.current) {
      clearInterval(intervalRef.current)
      intervalRef.current = null
    }

    if (!startedAt) {
      setElapsed(0)
      return
    }

    const startMs = new Date(startedAt).getTime()
    if (Number.isNaN(startMs)) {
      setElapsed(0)
      return
    }

    if (running) {
      // Tick every second while the task is running.
      const tick = () => setElapsed(Date.now() - startMs)
      tick() // immediate first value
      intervalRef.current = setInterval(tick, 1000)
    } else {
      // Task finished — compute final elapsed once.
      const endMs = finishedAt ? new Date(finishedAt).getTime() : Date.now()
      setElapsed(Number.isNaN(endMs) ? 0 : Math.max(0, endMs - startMs))
    }

    return () => {
      if (intervalRef.current) {
        clearInterval(intervalRef.current)
        intervalRef.current = null
      }
    }
  }, [startedAt, running, finishedAt])

  return elapsed
}
