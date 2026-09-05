import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ImagePlus, Loader2, AlertTriangle, CheckCircle2, XCircle, RotateCcw, Square, FileText, Timer, ChevronDown, ChevronRight, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Progress } from '@/components/ui/progress'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { cn } from '@/lib/utils'
import { formatDuration } from '@/lib/format'
import { useElapsed } from '@/lib/use-elapsed'
import { ConfirmDialog } from '@/components/common/ConfirmDialog'
import {
  createImageAnalysisBatch,
  getImageAnalysisBatch,
  getImageAnalysisResult,
  getImageAnalysisModels,
  getRecentImageAnalysisBatches,
  retryImageAnalysisBatch,
  abortImageAnalysisBatch,
  reExtractImageAnalysisBatch,
  cleanupImageAnalysisImages,
  streamImageAnalysisBatch,
  type AnalysisIntent,
  type BatchState,
  type BatchStatus,
  type BatchResult,
  type ProgressEvent,
  type ModelProfileOption,
} from '@/lib/api-client/image-analysis'

const ALL_INTENTS: AnalysisIntent[] = ['outline', 'progress', 'inspiration', 'state', 'lore']
const ACCEPTED_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'application/pdf']

// Maximum single upload size for image analysis batches.
// Must be ≤ backend MaxRequestBodySize (512MB).
const MAX_SINGLE_UPLOAD_BYTES = 1024 * 1024 * 1024

function formatSize(bytes: number): string {
  return (bytes / (1024 * 1024)).toFixed(1)
}

/** Whether a file is a PDF (by MIME type or extension fallback). */
function isPdfFile(f: File): boolean {
  return f.type === 'application/pdf' || f.name.toLowerCase().endsWith('.pdf')
}

/** Split files into chunks where each chunk's total size ≤ limit. */
function chunkFiles(files: File[], limit: number): File[][] {
  const chunks: File[][] = []
  let current: File[] = []
  let currentSize = 0
  for (const f of files) {
    if (f.size > limit) {
      // Single file exceeds limit — put it in its own chunk so we can
      // report the specific error later.
      if (current.length > 0) {
        chunks.push(current)
        current = []
        currentSize = 0
      }
      chunks.push([f])
      continue
    }
    if (currentSize + f.size > limit && current.length > 0) {
      chunks.push(current)
      current = []
      currentSize = 0
    }
    current.push(f)
    currentSize += f.size
  }
  if (current.length > 0) chunks.push(current)
  return chunks
}

/** Merge results from multiple batch chunks into a single BatchResult. */
function mergeBatchResults(results: BatchResult[]): BatchResult {
  if (results.length === 0)
    return { batch_id: '', status: 'completed', total_pages: 0, success_count: 0, failed_count: 0, sections: {} }
  const merged: BatchResult = {
    batch_id: results.map((r) => r.batch_id).join(','),
    status: 'completed',
    total_pages: 0,
    success_count: 0,
    failed_count: 0,
    sections: {} as Record<string, string>,
    failed_pages: [],
  }
  for (const r of results) {
    merged.total_pages += r.total_pages
    merged.success_count += r.success_count
    merged.failed_count += r.failed_count
    if (r.failed_pages) merged.failed_pages!.push(...r.failed_pages)
    for (const [intent, content] of Object.entries(r.sections)) {
      const key = intent as AnalysisIntent
      merged.sections[key] = (merged.sections[key] || '') + '\n' + (content || '')
    }
  }
  if (merged.failed_count === merged.total_pages) merged.status = 'failed'
  else if (merged.failed_count > 0) merged.status = 'partial'
  return merged
}

export function ImageAnalysisPanel() {
  const { t } = useTranslation()
  const [files, setFiles] = useState<File[]>([])
  const [intents, setIntents] = useState<AnalysisIntent[]>([...ALL_INTENTS])
  const [batch, setBatch] = useState<BatchState | null>(null)
  const [result, setResult] = useState<BatchResult | null>(null)
  const [uploading, setUploading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const [models, setModels] = useState<ModelProfileOption[]>([])
  const [selectedModelId, setSelectedModelId] = useState<string>('')
  const [showPages, setShowPages] = useState(false)
  const [reExtractModelId, setReExtractModelId] = useState<string>('')
  const [cleanupConfirmOpen, setCleanupConfirmOpen] = useState(false)
  const [pdfPageStart, setPdfPageStart] = useState<string>('')
  const [pdfPageEnd, setPdfPageEnd] = useState<string>('')
  const fileInputRef = useRef<HTMLInputElement>(null)
  const abortRef = useRef<AbortController | null>(null)
  const restoredRef = useRef(false)

  // Cleanup SSE on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  // Load available model profiles on mount and auto-select the default image model.
  useEffect(() => {
    getImageAnalysisModels()
      .then((list) => {
        setModels(list)
        // Auto-select the first profile marked as default for image analysis.
        const defaultModel = list.find((m) => m.default_for_image_analysis)
        if (defaultModel) {
          setSelectedModelId(defaultModel.id)
        }
      })
      .catch(() => {})
  }, [])

  // Restore the most recent batch state after a page refresh.
  // If the batch is still running, reconnect SSE; otherwise just display it.
  useEffect(() => {
    if (restoredRef.current) return
    restoredRef.current = true
    getRecentImageAnalysisBatches(1)
      .then((batches) => {
        if (batches.length === 0) return
        const latest = batches[0]
        setBatch(latest)
        // Load result for completed/partial batches.
        if (latest.status === 'completed' || latest.status === 'partial') {
          getImageAnalysisResult(latest.id).then(setResult).catch(() => {})
        }
        // If still running, reconnect to the SSE stream.
        if (latest.status === 'running' || latest.status === 'pending') {
          abortRef.current?.abort()
          abortRef.current = streamImageAnalysisBatch(
            latest.id,
            (event: ProgressEvent) => {
              const isDone =
                event.status === 'completed' ||
                event.status === 'partial' ||
                event.status === 'failed'
              if (isDone) {
                getImageAnalysisBatch(latest.id).then(setBatch).catch(() => {})
                getImageAnalysisResult(latest.id).then(setResult).catch(() => {})
                abortRef.current?.abort()
                return
              }
              setBatch((prev) =>
                prev ? { ...prev, status: event.status, current_page: event.current_page } : prev,
              )
            },
            () => {
              // SSE connection lost (e.g. backend crash/restart) — re-fetch
              // the batch state to pick up crash-recovery status changes.
              getImageAnalysisBatch(latest.id)
                .then((fresh) => {
                  setBatch(fresh)
                  if (fresh.status === 'completed' || fresh.status === 'partial') {
                    getImageAnalysisResult(fresh.id).then(setResult).catch(() => {})
                  }
                })
                .catch(() => {
                  // Backend still unreachable — mark as failed locally so
                  // the user isn't stuck on a perpetual "running" spinner.
                  setBatch((prev) =>
                    prev ? { ...prev, status: 'failed' } : prev,
                  )
                })
            },
          )
        }
      })
      .catch(() => {})
  }, [])

  const handleFiles = useCallback((newFiles: FileList | File[]) => {
    const arr = Array.from(newFiles)
    const valid = arr.filter((f) => ACCEPTED_TYPES.includes(f.type) || isPdfFile(f))
    if (valid.length === 0) return

    // Warn about single-file size.
    const oversized = valid.find((f) => f.size > MAX_SINGLE_UPLOAD_BYTES)
    if (oversized) {
      setError(
        t('imageAnalysis.error.singleFileTooLarge', {
          name: oversized.name,
          size: formatSize(oversized.size),
        }),
      )
      return
    }

    setFiles((prev) => [...prev, ...valid])
    setError(null)
  }, [t])

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault()
      setDragOver(false)
      if (e.dataTransfer.files.length > 0) handleFiles(e.dataTransfer.files)
    },
    [handleFiles],
  )

  const toggleIntent = (intent: AnalysisIntent) => {
    setIntents((prev) => (prev.includes(intent) ? prev.filter((i) => i !== intent) : [...prev, intent]))
  }

  const startAnalysis = async () => {
    if (files.length === 0) {
      setError(t('imageAnalysis.error.noImages'))
      return
    }

    const chunks = chunkFiles(files, MAX_SINGLE_UPLOAD_BYTES)
    console.log(
      '[image-analysis] startAnalysis: total files=%d, total chunks=%d, chunks=%s',
      files.length,
      chunks.length,
      chunks.map((c) => c.length).join(','),
    )

    // If any single-file chunk exists, show error immediately.
    const oversizedChunk = chunks.find((c) => c.length === 1 && c[0].size > MAX_SINGLE_UPLOAD_BYTES)
    if (oversizedChunk) {
      const f = oversizedChunk[0]
      setError(
        t('imageAnalysis.error.singleFileTooLarge', { name: f.name, size: formatSize(f.size) }),
      )
      return
    }

    setUploading(true)
    setError(null)
    setResult(null)

    const allResults: BatchResult[] = []
    const chunkErrors: string[] = []
    // PDF page range (parsed from the optional inputs; only sent for PDF chunks).
    const pageRange = {
      start: pdfPageStart ? parseInt(pdfPageStart, 10) : undefined,
      end: pdfPageEnd ? parseInt(pdfPageEnd, 10) : undefined,
    }
    try {
      for (let ci = 0; ci < chunks.length; ci++) {
        const chunk = chunks[ci]
        // offset = total pages from all previous chunks
        const offset = chunks.slice(0, ci).reduce((sum, c) => sum + c.length, 0)
        console.log(
          '[image-analysis] chunk %d/%d: files=%d, offset=%d, first=%s',
          ci + 1,
          chunks.length,
          chunk.length,
          offset,
          chunk[0]?.name,
        )

        let state: BatchState
        try {
          const chunkHasPdf = chunk.some(isPdfFile)
          state = await createImageAnalysisBatch(
            chunk,
            intents,
            selectedModelId || undefined,
            chunkHasPdf ? pageRange : undefined,
          )
        } catch (uploadErr) {
          const msg = uploadErr instanceof Error ? uploadErr.message : String(uploadErr)
          console.error('[image-analysis] chunk %d upload failed: %s', ci + 1, msg)
          chunkErrors.push(`第 ${ci + 1} 包上传失败: ${msg}`)
          break // stop processing remaining chunks
        }

        if (ci === 0) {
          setFiles([])
        }

        // Build a synthetic batch state that aggregates over all chunks.
        const mergedState: BatchState = {
          ...state,
          total_pages: files.length,
          current_page: offset,
        }
        setBatch(mergedState)

        // Wait for this chunk to finish via SSE.
        await new Promise<void>((resolve, reject) => {
          abortRef.current?.abort()
          const ctrl = streamImageAnalysisBatch(
            state.id,
            (event: ProgressEvent) => {
              const isDone =
                event.status === 'completed' ||
                event.status === 'partial' ||
                event.status === 'failed'

              if (isDone) {
                // Wait for both the final state and the result before resolving,
                // so that chunk sequencing is deterministic and results are never lost.
                Promise.all([
                  getImageAnalysisBatch(state.id),
                  getImageAnalysisResult(state.id),
                ])
                  .then(([finalState, chunkResult]) => {
                    setBatch((prev) => {
                      if (!prev) return prev
                      return {
                        ...prev,
                        ...finalState,
                        current_page: offset + finalState.current_page,
                        total_pages: files.length,
                      }
                    })
                    allResults.push(chunkResult)
                    setResult(mergeBatchResults(allResults))
                    console.log(
                      '[image-analysis] chunk %d done: batch=%s pages=%d success=%d failed=%d',
                      ci + 1,
                      finalState.id,
                      chunkResult.total_pages,
                      chunkResult.success_count,
                      chunkResult.failed_count,
                    )
                  })
                  .catch((err) => {
                    console.error('[image-analysis] chunk %d result fetch failed: %s', ci + 1, err)
                    chunkErrors.push(`第 ${ci + 1} 包结果获取失败`)
                  })
                  .finally(() => {
                    ctrl.abort()
                    resolve()
                  })
                return
              }

              // Progress update — only for image_analysis_progress events which
              // have valid current_page.
              setBatch((prev) => {
                if (!prev) return prev
                return {
                  ...prev,
                  status: event.status,
                  current_page: offset + event.current_page,
                  total_pages: files.length,
                }
              })
            },
            (err) => {
              console.error('[image-analysis] chunk %d SSE error: %s', ci + 1, err)
              reject(err)
            },
          )
          abortRef.current = ctrl
        })
      }

      // All chunks done — set final aggregated status.
      const anyFailed = allResults.some((r) => r.status === 'failed' || r.status === 'partial')
      const allFailed = allResults.length > 0 && allResults.every((r) => r.status === 'failed')
      const finalStatus = allFailed ? 'failed' : anyFailed ? 'partial' : 'completed'
      setBatch((prev) =>
        prev
          ? {
              ...prev,
              status: finalStatus as BatchStatus,
            }
          : prev,
      )

      // Surface chunk-level errors that don't stop the whole flow.
      if (chunkErrors.length > 0) {
        setError(chunkErrors.join('；'))
      }
    } catch (err) {
      console.error('[image-analysis] startAnalysis fatal error:', err)
      const isNetworkError =
        err instanceof TypeError && err.message === 'Failed to fetch'
      if (isNetworkError) {
        setError(t('imageAnalysis.error.networkError'))
      } else {
        setError(err instanceof Error ? err.message : t('imageAnalysis.error.uploadFailed'))
      }
    } finally {
      setUploading(false)
    }
  }

  const handleRetry = async () => {
    if (!batch) return
    try {
      await retryImageAnalysisBatch(batch.id)
      setResult(null)
      abortRef.current?.abort()
      abortRef.current = streamImageAnalysisBatch(
        batch.id,
        (event: ProgressEvent) => {
          const isDone =
            event.status === 'completed' || event.status === 'partial'
          if (isDone) {
            getImageAnalysisBatch(batch.id).then(setBatch).catch(() => {})
            getImageAnalysisResult(batch.id).then(setResult).catch(() => {})
            abortRef.current?.abort()
            return
          }
          // Progress — only image_analysis_progress has valid current_page.
          setBatch((prev) =>
            prev ? { ...prev, status: event.status, current_page: event.current_page } : prev,
          )
        },
        (err) => setError(err.message),
      )
    } catch (err) {
      setError(err instanceof Error ? err.message : t('imageAnalysis.error.batchFailed'))
    }
  }

  const handleAbort = async () => {
    if (!batch) return
    try {
      await abortImageAnalysisBatch(batch.id)
      abortRef.current?.abort()
      // Re-fetch actual state — backend may mark stale batches as "failed"
      // rather than "aborted" when there's no in-memory task to cancel.
      getImageAnalysisBatch(batch.id)
        .then((fresh) => {
          setBatch(fresh)
          if (fresh.status === 'completed' || fresh.status === 'partial') {
            getImageAnalysisResult(fresh.id).then(setResult).catch(() => {})
          }
        })
        .catch(() => {
          // Fallback: assume aborted if we can't reach the backend.
          setBatch((prev) => (prev ? { ...prev, status: 'aborted' } : prev))
        })
    } catch {
      // ignore
    }
  }

  const handleReExtract = async () => {
    if (!batch || !reExtractModelId) return
    try {
      setUploading(true)
      setError(null)
      const newState = await reExtractImageAnalysisBatch(batch.id, reExtractModelId)
      setBatch(newState)
      setResult(null)
      setShowPages(false)
      abortRef.current?.abort()
      abortRef.current = streamImageAnalysisBatch(
        newState.id,
        (event: ProgressEvent) => {
          const isDone =
            event.status === 'completed' || event.status === 'partial' || event.status === 'failed'
          if (isDone) {
            getImageAnalysisBatch(newState.id).then(setBatch).catch(() => {})
            getImageAnalysisResult(newState.id).then(setResult).catch(() => {})
            abortRef.current?.abort()
            return
          }
          setBatch((prev) =>
            prev ? { ...prev, status: event.status, current_page: event.current_page } : prev,
          )
        },
        (err) => setError(err.message),
      )
    } catch (err) {
      setError(err instanceof Error ? err.message : '重新提取失败')
    } finally {
      setUploading(false)
    }
  }

  // 手动清理原始图片：批次结束后用户确认不再重测/重试时回收磁盘空间。
  const handleCleanupImages = async () => {
    if (!batch) return
    try {
      await cleanupImageAnalysisImages(batch.id)
      setBatch((prev) => (prev ? { ...prev, images_deleted: true } : prev))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('imageAnalysis.cleanupFailed'))
    }
  }

  const isRunning = batch?.status === 'running' || batch?.status === 'pending'
  const progressPercent = batch && batch.total_pages > 0 ? Math.round((batch.current_page / batch.total_pages) * 100) : 0

  // Real-time elapsed timer — reuses the same formatDuration as Agent Trace.
  const elapsedMs = useElapsed(batch?.started_at, isRunning, batch?.finished_at)

  return (
    <div className="flex h-full flex-col gap-4 overflow-y-auto p-4">
      {/* Header */}
      <div>
        <h2 className="text-lg font-semibold">{t('imageAnalysis.title')}</h2>
        <p className="text-sm text-muted-foreground">{t('imageAnalysis.subtitle')}</p>
      </div>

      {/* Upload area (only when no active batch) */}
      {!batch && (
        <>
          <div
            className={cn(
              'flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed p-8 transition-colors',
              dragOver ? 'border-primary bg-primary/5' : 'border-muted-foreground/25 hover:border-primary/50',
            )}
            onClick={() => fileInputRef.current?.click()}
            onDragOver={(e) => {
              e.preventDefault()
              setDragOver(true)
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={handleDrop}
          >
            <ImagePlus className="h-8 w-8 text-muted-foreground" />
            <span className="text-sm font-medium">{t('imageAnalysis.upload')}</span>
            <span className="text-xs text-muted-foreground">{t('imageAnalysis.uploadHint')}</span>
            <input
              ref={fileInputRef}
              type="file"
              accept={ACCEPTED_TYPES.join(',')}
              multiple
              className="hidden"
              onChange={(e) => e.target.files && handleFiles(e.target.files)}
            />
          </div>

          {/* Selected files preview */}
          {files.length > 0 && (
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">
                  {files.length} 张图片
                </span>
                <Button variant="ghost" size="sm" onClick={() => setFiles([])}>
                  清除
                </Button>
              </div>
              <div className="grid grid-cols-5 gap-2">
                {files.slice(0, 20).map((file, i) => (
                  <div key={i} className="relative aspect-square overflow-hidden rounded-md border">
                    {isPdfFile(file) ? (
                      <div className="flex h-full w-full flex-col items-center justify-center gap-1 bg-muted/40">
                        <FileText className="h-6 w-6 text-muted-foreground" />
                        <span className="px-1 text-center text-[9px] text-muted-foreground leading-tight line-clamp-2">
                          {file.name}
                        </span>
                      </div>
                    ) : (
                      <img
                        src={URL.createObjectURL(file)}
                        alt={file.name}
                        className="h-full w-full object-cover"
                      />
                    )}
                    <span className="absolute bottom-0 left-0 right-0 bg-black/60 px-1 text-[10px] text-white truncate">
                      {i + 1}
                    </span>
                  </div>
                ))}
                {files.length > 20 && (
                  <div className="flex aspect-square items-center justify-center rounded-md border text-xs text-muted-foreground">
                    +{files.length - 20}
                  </div>
                )}
              </div>

              {/* PDF page range (shown only when a PDF is selected) */}
              {files.some(isPdfFile) && (
                <div className="flex items-center gap-2 rounded-md border border-dashed p-2">
                  <FileText className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="text-xs text-muted-foreground">{t('imageAnalysis.pdfPageRange')}</span>
                  <input
                    type="number"
                    min={1}
                    value={pdfPageStart}
                    onChange={(e) => setPdfPageStart(e.target.value)}
                    placeholder="1"
                    className="h-7 w-16 rounded border bg-background px-2 text-xs"
                  />
                  <span className="text-xs text-muted-foreground">–</span>
                  <input
                    type="number"
                    min={1}
                    value={pdfPageEnd}
                    onChange={(e) => setPdfPageEnd(e.target.value)}
                    placeholder="∞"
                    className="h-7 w-16 rounded border bg-background px-2 text-xs"
                  />
                  <span className="text-[10px] text-muted-foreground">{t('imageAnalysis.pdfPageRangeHint')}</span>
                </div>
              )}
            </div>
          )}

          {/* Intent selection + model selection */}
          <div className="space-y-2">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium">{t('imageAnalysis.intents')}</span>
              {models.length > 0 && (
                <Select value={selectedModelId} onValueChange={setSelectedModelId}>
                  <SelectTrigger className="h-8 w-48 text-xs">
                    <SelectValue placeholder={t('imageAnalysis.modelDefault')} />
                  </SelectTrigger>
                  <SelectContent>
                    {models.map((m) => (
                      <SelectItem key={m.id} value={m.id}>
                        {m.name}{m.model ? ` (${m.model})` : ''}{m.default_for_image_analysis ? ' 🖼' : ''}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </div>
            <div className="flex flex-wrap gap-2">
              {ALL_INTENTS.map((intent) => (
                <Badge
                  key={intent}
                  variant={intents.includes(intent) ? 'default' : 'outline'}
                  className="cursor-pointer select-none"
                  onClick={() => toggleIntent(intent)}
                >
                  {t(`imageAnalysis.intent.${intent}`)}
                </Badge>
              ))}
            </div>
          </div>

          {/* Start button */}
          <Button onClick={startAnalysis} disabled={files.length === 0 || uploading || intents.length === 0}>
            {uploading ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <ImagePlus className="mr-2 h-4 w-4" />}
            {uploading ? t('imageAnalysis.uploading') : t('imageAnalysis.start')}
          </Button>
        </>
      )}

      {/* Active batch progress */}
      {batch && (
        <div className="space-y-4">
          {/* Status header */}
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              {isRunning && <Loader2 className="h-4 w-4 animate-spin text-primary" />}
              {batch.status === 'completed' && <CheckCircle2 className="h-4 w-4 text-green-500" />}
              {batch.status === 'partial' && <AlertTriangle className="h-4 w-4 text-yellow-500" />}
              {batch.status === 'failed' && <XCircle className="h-4 w-4 text-destructive" />}
              {batch.status === 'aborted' && <Square className="h-4 w-4 text-muted-foreground" />}
              <span className="text-sm font-medium">
                {batch.status === 'running' || batch.status === 'pending'
                  ? t('imageAnalysis.processing', { current: batch.current_page + 1, total: batch.total_pages })
                  : batch.status === 'completed'
                    ? t('imageAnalysis.completed')
                    : batch.status === 'partial'
                      ? t('imageAnalysis.partial', { failed: batch.results?.filter((r) => r.status === 'failed').length ?? 0 })
                      : batch.status === 'failed'
                        ? t('imageAnalysis.failed')
                        : t('imageAnalysis.aborted')}
              </span>
              {batch.started_at && elapsedMs > 0 && (
                <span className="inline-flex items-center gap-1 rounded border border-[var(--nova-border-soft)] px-1.5 py-0.5 font-mono text-[10px] text-[var(--nova-text-faint)]">
                  <Timer className="h-3 w-3" />
                  {formatDuration(elapsedMs)}
                </span>
              )}
            </div>
            <div className="flex items-center gap-1">
              {(batch.status === 'partial' || batch.status === 'failed') && !batch.images_deleted && (
                <Button variant="ghost" size="sm" onClick={handleRetry}>
                  <RotateCcw className="mr-1 h-3 w-3" />
                  {t('imageAnalysis.retry')}
                </Button>
              )}
              {isRunning && (
                <Button variant="ghost" size="sm" onClick={handleAbort}>
                  <Square className="mr-1 h-3 w-3" />
                  {t('imageAnalysis.abort')}
                </Button>
              )}
              {!isRunning && !batch.images_deleted && (
                <Button variant="ghost" size="sm" onClick={() => setCleanupConfirmOpen(true)}>
                  <Trash2 className="mr-1 h-3 w-3" />
                  {t('imageAnalysis.cleanupImages')}
                </Button>
              )}
            </div>
          </div>

          {/* Progress bar */}
          {isRunning && <Progress value={progressPercent} className="h-2" />}

          {/* Page results grid — collapsible */}
          {batch.results && batch.results.length > 0 && (
            <div>
              <button
                className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors"
                onClick={() => setShowPages(!showPages)}
              >
                {showPages ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                {showPages ? '折叠页面状态' : '展开页面状态'} ({batch.results.filter((r) => r.status === 'success').length}/{batch.results.length})
              </button>
              {showPages && (
                <div className="mt-2 grid grid-cols-5 gap-1.5">
                  {batch.results.map((page) => (
                    <div
                      key={page.page_index}
                      className={cn(
                        'flex aspect-square items-center justify-center rounded-md border text-xs',
                        page.status === 'success' && 'border-green-500/50 bg-green-500/10 text-green-600',
                        page.status === 'failed' && 'border-destructive/50 bg-destructive/10 text-destructive',
                        page.status === 'processing' && 'border-primary/50 bg-primary/10 text-primary',
                        page.status === 'pending' && 'border-muted-foreground/25 text-muted-foreground',
                      )}
                      title={`${page.file_name}: ${t(`imageAnalysis.pageStatus.${page.status}`)}`}
                    >
                      {page.status === 'success' ? '✓' : page.status === 'failed' ? '✗' : page.page_index + 1}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Result preview */}
          {result && (
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <FileText className="h-4 w-4" />
                <span className="text-sm font-medium">{t('imageAnalysis.result')}</span>
              </div>
              <div className="max-h-64 overflow-y-auto rounded-md border bg-muted/30 p-3 text-xs">
                {Object.entries(result.sections).map(([intent, content]) => (
                  <div key={intent} className="mb-3">
                    <div className="mb-1 font-medium capitalize">{intent}</div>
                    <pre className="whitespace-pre-wrap font-sans text-muted-foreground">{content}</pre>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Re-extract with different model — requires raw images to still exist */}
          {(batch.status === 'partial' || batch.status === 'failed' || batch.status === 'completed' || batch.status === 'aborted') && !isRunning && !batch.images_deleted && (
            <div className="flex items-center gap-2 rounded-md border border-muted-foreground/20 bg-muted/20 p-2">
              <span className="text-xs text-muted-foreground shrink-0">切换模型</span>
              <Select value={reExtractModelId} onValueChange={setReExtractModelId}>
                <SelectTrigger className="h-7 w-40 text-xs">
                  <SelectValue placeholder="选择模型" />
                </SelectTrigger>
                <SelectContent>
                  {models.map((m) => (
                    <SelectItem key={m.id} value={m.id}>
                      {m.name}{m.default_for_image_analysis ? ' 🖼' : ''}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Button
                variant="outline"
                size="sm"
                onClick={handleReExtract}
                disabled={!reExtractModelId || uploading}
              >
                {uploading ? <Loader2 className="mr-1 h-3 w-3 animate-spin" /> : <RotateCcw className="mr-1 h-3 w-3" />}
                重新提取
              </Button>
            </div>
          )}

          {/* 原始图片已清理：重试/重新提取不可用提示 */}
          {!isRunning && batch.images_deleted && (
            <p className="text-xs text-muted-foreground">{t('imageAnalysis.imagesDeleted')}</p>
          )}

          {/* Action buttons */}
          <div className="flex gap-2">
            {(batch.status === 'completed' || batch.status === 'partial' || batch.status === 'aborted' || batch.status === 'failed') && (
              <Button variant="outline" size="sm" onClick={() => { setBatch(null); setResult(null); setShowPages(false) }}>
                新批次
              </Button>
            )}
          </div>

          {/* Review hint — tell user how to review results in Agent chat */}
          {(batch.status === 'completed' || batch.status === 'partial') && result && (
            <div className="rounded-md border border-primary/30 bg-primary/5 p-3 text-xs space-y-1">
              <div className="font-medium text-primary">{t('imageAnalysis.reviewHint')}</div>
              <div className="text-muted-foreground">{t('imageAnalysis.reviewHintDesc')}</div>
              <code className="block rounded bg-muted px-2 py-1 font-mono text-[11px] select-all">
                @assets/analysis/batch-{result.batch_id}/result.md 审核提取结果
              </code>
            </div>
          )}
        </div>
      )}

      {/* Error */}
      {error && (
        <div className="flex items-center gap-2 rounded-md border border-destructive/50 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          {error}
        </div>
      )}

      {/* 手动清理确认：图片删除后重试/重新提取不可用 */}
      <ConfirmDialog
        open={cleanupConfirmOpen}
        onOpenChange={setCleanupConfirmOpen}
        title={t('imageAnalysis.cleanupConfirmTitle')}
        description={t('imageAnalysis.cleanupConfirmDesc')}
        confirmLabel={t('imageAnalysis.cleanupImages')}
        tone="danger"
        onConfirm={handleCleanupImages}
      />
    </div>
  )
}
