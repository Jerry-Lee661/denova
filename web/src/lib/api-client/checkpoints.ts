import { jsonHeaders, requestJSON } from './client'
import type { Checkpoint, CheckpointRestoreResult, SessionTruncateResult } from './types'

export async function getCheckpoints(sessionID = ''): Promise<Checkpoint[]> {
  const query = sessionID ? `?session_id=${encodeURIComponent(sessionID)}` : ''
  const data = await requestJSON<{ checkpoints: Checkpoint[] }>(`/api/checkpoints${query}`)
  return data.checkpoints || []
}

export async function createCheckpoint(sessionID = '', reason = ''): Promise<Checkpoint> {
  return requestJSON<Checkpoint>('/api/checkpoints', {
    method: 'POST',
    headers: jsonHeaders,
    body: JSON.stringify({ session_id: sessionID, reason }),
  })
}

export async function restoreCheckpoint(id: string): Promise<CheckpointRestoreResult> {
  return requestJSON<CheckpointRestoreResult>('/api/checkpoints/restore', {
    method: 'POST',
    headers: jsonHeaders,
    body: JSON.stringify({ id }),
  })
}

export async function truncateSession(sessionID: string, messageIndex: number, reason = ''): Promise<SessionTruncateResult> {
  return requestJSON<SessionTruncateResult>('/api/session/truncate', {
    method: 'POST',
    headers: jsonHeaders,
    body: JSON.stringify({ session_id: sessionID, message_index: messageIndex, reason }),
  })
}
