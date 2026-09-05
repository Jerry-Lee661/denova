import { fetchAPI, requestJSON } from './client'

// --- Types ---

export type AnalysisIntent = 'outline' | 'progress' | 'inspiration' | 'state' | 'lore'

export type BatchStatus = 'pending' | 'running' | 'completed' | 'failed' | 'aborted' | 'partial'

export interface ImageItem {
  page_index: number
  file_name: string
  storage_path: string
  mime_type: string
  size_bytes: number
}

export interface ExtractedItem {
  type: 'character' | 'scene' | 'plot_point' | 'world_building' | 'dialogue' | 'other'
  content: string
}

export interface PageResult {
  page_index: number
  file_name: string
  status: 'success' | 'failed' | 'pending' | 'processing'
  error?: string
  content?: string
  items?: ExtractedItem[]
  retried_at?: string
}

export interface BatchState {
  id: string
  status: BatchStatus
  intents: AnalysisIntent[]
  images: ImageItem[]
  results: PageResult[]
  created_at: string
  started_at?: string
  finished_at?: string
  current_page: number
  total_pages: number
  model_id?: string
  /** 原始上传图片已删除（保留期清扫或手动清理）；重试/重新提取不可用 */
  images_deleted?: boolean
}

export interface BatchResult {
  batch_id: string
  status: BatchStatus
  total_pages: number
  success_count: number
  failed_count: number
  sections: Partial<Record<AnalysisIntent, string>>
  fact_stream?: string // 按页序拼接的完整事实流，供跨页整合模型使用
  failed_pages?: PageResult[]
}

export interface ProgressEvent {
  batch_id: string
  status: BatchStatus
  current_page: number
  total_pages: number
  file_name?: string
  preview?: string
  error?: string
}

export interface ModelProfileOption {
  id: string
  name: string
  model: string
  default_for_image_analysis: boolean
}

// --- API functions ---

/** Fetch available model profiles for image analysis. */
export function getImageAnalysisModels(): Promise<ModelProfileOption[]> {
  return requestJSON('/api/image-analysis/models')
}

/** Fetch recent batch states for panel state restoration after refresh. */
export function getRecentImageAnalysisBatches(limit = 10): Promise<BatchState[]> {
  return requestJSON(`/api/image-analysis/batches/recent?limit=${limit}`)
}

/** Submit a batch of images for analysis. */
export async function createImageAnalysisBatch(
  files: File[],
  intents: AnalysisIntent[] = [],
  modelId?: string,
  pageRange?: { start?: number; end?: number },
): Promise<BatchState> {
  const form = new FormData()
  for (const file of files) {
    form.append('images', file)
  }
  if (intents.length > 0) {
    form.append('intents', intents.join(','))
  }
  if (modelId) {
    form.append('model_id', modelId)
  }
  // PDF page range (1-based, inclusive). Only meaningful when a PDF is uploaded.
  if (pageRange?.start && pageRange.start > 0) {
    form.append('page_start', String(pageRange.start))
  }
  if (pageRange?.end && pageRange.end > 0) {
    form.append('page_end', String(pageRange.end))
  }
  const res = await fetchAPI('/api/image-analysis/batch', {
    method: 'POST',
    body: form,
  })
  if (!res.ok) {
    const text = await res.text()
    throw new Error(text || `HTTP ${res.status}`)
  }
  return res.json()
}

/** Get the current state of a batch. */
export function getImageAnalysisBatch(batchId: string): Promise<BatchState> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}`)
}

/** Get the aggregated result of a batch. */
export function getImageAnalysisResult(batchId: string): Promise<BatchResult> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/result`)
}

/** Retry failed pages in a batch. */
export function retryImageAnalysisBatch(
  batchId: string,
  pageIndices: number[] = [],
): Promise<{ status: string }> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/retry`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ page_indices: pageIndices }),
  })
}

/** Re-extract: create a new batch reusing uploaded images with a different model. */
export function reExtractImageAnalysisBatch(
  batchId: string,
  modelId: string,
): Promise<BatchState> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/re-extract`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model_id: modelId }),
  })
}

/** Abort a running batch. */
export function abortImageAnalysisBatch(batchId: string): Promise<{ status: string }> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/abort`, {
    method: 'POST',
  })
}

/** Manually delete the raw uploaded images of a finished batch. */
export function cleanupImageAnalysisImages(batchId: string): Promise<{ status: string }> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/cleanup-images`, {
    method: 'POST',
  })
}

/** Apply batch results (returns result for agent/frontend consumption). */
export function applyImageAnalysisBatch(batchId: string): Promise<BatchResult> {
  return requestJSON(`/api/image-analysis/batch/${encodeURIComponent(batchId)}/apply`, {
    method: 'POST',
  })
}

/** Subscribe to batch progress via SSE. Returns an EventSource-like reader. */
export function streamImageAnalysisBatch(
  batchId: string,
  onEvent: (event: ProgressEvent) => void,
  onError?: (error: Error) => void,
): AbortController {
  const controller = new AbortController()
  const url = `/api/image-analysis/batch/${encodeURIComponent(batchId)}/stream`

  fetchAPI(url, { signal: controller.signal })
    .then(async (res) => {
      if (!res.ok || !res.body) {
        throw new Error(`SSE connection failed: HTTP ${res.status}`)
      }
      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() ?? ''
        for (const line of lines) {
          if (line.startsWith('data: ')) {
            try {
              const data = JSON.parse(line.slice(6))
              if (data.type === 'image_analysis_progress') {
                // Progress event: valid ProgressEvent with current_page.
                onEvent(data.data as ProgressEvent)
              } else if (data.type === 'image_analysis_done') {
                // Done event: data is BatchResult (no current_page).
                // Signal completion via status so the caller fetches final state.
                const result = data.data as BatchResult
                onEvent({
                  batch_id: result.batch_id,
                  status: result.status,
                  current_page: 0, // will be overwritten by API fetch
                  total_pages: result.total_pages,
                })
              }
            } catch {
              // skip malformed SSE lines
            }
          }
        }
      }
    })
    .catch((err) => {
      if (err.name !== 'AbortError') {
        onError?.(err)
      }
    })

  return controller
}
