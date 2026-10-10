import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { LoreClassificationPreview } from '@/lib/api'
import { LoreClassificationDialog } from './LoreClassificationDialog'

const previewLoreClassificationMock = vi.hoisted(() => vi.fn())
const applyLoreClassificationMock = vi.hoisted(() => vi.fn())

vi.mock('@/lib/api', () => ({
  previewLoreClassification: previewLoreClassificationMock,
  applyLoreClassification: applyLoreClassificationMock,
}))

function previewOf(sources: Array<'heuristic' | 'semantic'>): LoreClassificationPreview {
  return {
    revision: 'revision-1',
    mode: 'semantic',
    counts: {},
    items: sources.map((source, index) => ({
      id: `item-${index}`,
      name: `条目 ${index}`,
      current_type: 'other',
      current_type_source: 'heuristic',
      suggested_type: source === 'semantic' ? 'character' : 'other',
      confidence: 'high',
      suggestion_source: source,
    })),
  }
}

describe('LoreClassificationDialog', () => {
  beforeEach(() => {
    previewLoreClassificationMock.mockReset()
    applyLoreClassificationMock.mockReset()
    previewLoreClassificationMock.mockResolvedValue(previewOf(['heuristic', 'semantic']))
  })

  it('reserves forced semantic analysis for the AI analysis action', async () => {
    render(<LoreClassificationDialog open projectId="project-1" onOpenChange={() => {}} onApplied={() => {}} />)
    await waitFor(() => expect(previewLoreClassificationMock).toHaveBeenCalledWith('project-1', { mode: 'semantic', force_semantic: false }))

    await userEvent.click(screen.getByRole('button', { name: 'AI 分析' }))
    await waitFor(() => expect(previewLoreClassificationMock).toHaveBeenLastCalledWith('project-1', { mode: 'semantic', force_semantic: true }))

    await userEvent.click(screen.getByRole('button', { name: '重新分析' }))
    await waitFor(() => expect(previewLoreClassificationMock).toHaveBeenLastCalledWith('project-1', { mode: 'semantic', force_semantic: false }))
  })
})
