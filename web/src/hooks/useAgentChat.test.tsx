import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getMessagesPage, getSessions, switchSession, truncateSession, type SessionSummary } from '@/lib/api'
import { buildAgentMessageViews } from '@/lib/agent-message-view'
import { useAgentChat } from './useAgentChat'

const chatMock = vi.hoisted(() => ({
  options: null as Record<string, any> | null,
  messages: [] as any[],
  sendMessage: vi.fn(),
  setMessages: vi.fn(),
  resumeStream: vi.fn(),
  stop: vi.fn(),
  status: 'ready' as 'ready' | 'submitted' | 'streaming',
}))

vi.mock('@ai-sdk/react', () => ({
  useChat: (options: Record<string, any>) => {
    chatMock.options = options
    return {
      messages: chatMock.messages,
      setMessages: chatMock.setMessages,
      sendMessage: chatMock.sendMessage,
      resumeStream: chatMock.resumeStream,
      stop: chatMock.stop,
      status: chatMock.status,
    }
  },
}))

vi.mock('@/lib/api', () => ({
  abortChat: vi.fn(),
  analyzeChatContext: vi.fn(),
  createSession: vi.fn(),
  deleteSession: vi.fn(),
  executeCommand: vi.fn(),
  executeTool: vi.fn().mockResolvedValue({ result: '', error: undefined }),
  getActiveChatTask: vi.fn().mockResolvedValue({ active: false }),
  getCheckpoints: vi.fn().mockResolvedValue([]),
  getMessagesPage: vi.fn().mockResolvedValue({ messages: [], nextBefore: '0', hasMore: false, total: 0 }),
  getSessions: vi.fn().mockResolvedValue([]),
  renameSession: vi.fn(),
  restoreCheckpoint: vi.fn(),
  switchSession: vi.fn(),
  truncateSession: vi.fn(),
}))

vi.mock('@/features/settings/api', () => ({
  fetchSettings: vi.fn().mockResolvedValue({ effective: {} }),
}))

describe('useAgentChat', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    chatMock.options = null
    chatMock.messages = []
    chatMock.status = 'ready'
  })

  it('stops the old stream and selects the target immediately when switching sessions', async () => {
    chatMock.status = 'streaming'
    let finishSwitch!: (session: SessionSummary) => void
    vi.mocked(switchSession).mockReturnValue(new Promise((resolve) => { finishSwitch = resolve }))
    vi.mocked(getSessions).mockResolvedValue([
      { id: 'target', title: 'just say hello', active: true, message_count: 17, created_at: '2026-07-02T13:26:00Z', updated_at: '2026-07-02T13:26:00Z' },
    ])
    vi.mocked(getMessagesPage).mockResolvedValue({ messages: [], nextBefore: '0', hasMore: false, total: 0 })
    const { result } = renderHook(() => useAgentChat())

    let request!: Promise<void>
    act(() => {
      request = result.current.switchChatSession('target')
    })

    expect(chatMock.stop).toHaveBeenCalledTimes(1)
    expect(result.current.activeSessionId).toBe('target')

    await act(async () => {
      finishSwitch({ id: 'target', title: 'just say hello', active: true, message_count: 17, created_at: '2026-07-02T13:26:00Z', updated_at: '2026-07-02T13:26:00Z' })
      await request
    })
  })

  it('moves the submitted reference snapshot into the user message immediately', async () => {
    let finishRequest!: () => void
    chatMock.sendMessage.mockReturnValue(new Promise<void>((resolve) => { finishRequest = resolve }))
    const onSubmissionStart = vi.fn()
    const { result } = renderHook(() => useAgentChat())

    act(() => {
      result.current.addReference('chapters/ch01.md')
      result.current.addLoreReference('character-1')
      result.current.addStyleScene('battle')
      result.current.addTextSelection({ fileName: 'chapters/ch02.md', startLine: 8, endLine: 10, content: '被引用的正文' })
    })

    let sendResult!: Promise<boolean>
    act(() => {
      sendResult = result.current.send('请统一修改', {
        reviewFeedback: [{ reviewThreadId: 'thread-1', commentIds: ['comment-1'] }],
        reviewFeedbackDisplay: {
          comments: [{ id: 'comment-1', body: '需要增加爽点', review_path: 'setting/progress.md', review_line: 24 }],
        },
        onSubmissionStart,
      })
    })

    expect(onSubmissionStart).toHaveBeenCalledTimes(1)
    expect(result.current.references).toEqual([])
    expect(result.current.loreReferences).toEqual([])
    expect(result.current.styleScenes).toEqual([])
    expect(result.current.textSelections).toEqual([])
    expect(chatMock.sendMessage).toHaveBeenCalledWith(
      expect.objectContaining({
        role: 'user',
        metadata: expect.objectContaining({
          user_references: expect.arrayContaining([
            expect.objectContaining({ kind: 'file', label: 'chapters/ch01.md' }),
            expect.objectContaining({ kind: 'lore', label: 'character-1' }),
            expect.objectContaining({ kind: 'style', label: 'battle' }),
            expect.objectContaining({ kind: 'selection', label: 'chapters/ch02.md', start_line: 8, end_line: 10 }),
            expect.objectContaining({ kind: 'review_comment', id: 'comment-1', label: 'setting/progress.md', start_line: 24, detail: '需要增加爽点' }),
          ]),
        }),
      }),
      expect.any(Object),
    )

    act(() => result.current.addReference('chapters/next.md'))
    act(() => chatMock.options?.onFinish?.())
    expect(result.current.references).toEqual(['chapters/next.md'])

    await act(async () => finishRequest())
    await expect(sendResult).resolves.toBe(true)
  })

  it('restores consumed composer references when submission fails', async () => {
    chatMock.sendMessage.mockRejectedValue(new Error('offline'))
    const onSubmissionError = vi.fn()
    const { result } = renderHook(() => useAgentChat())
    act(() => result.current.addReference('chapters/ch01.md'))

    await act(async () => {
      expect(await result.current.send('继续', { onSubmissionError })).toBe(false)
    })

    await waitFor(() => expect(result.current.references).toEqual(['chapters/ch01.md']))
    expect(onSubmissionError).toHaveBeenCalledTimes(1)
  })

  it('uses the latest plan mode when switching and sending immediately', async () => {
    chatMock.sendMessage.mockResolvedValue(undefined)
    const { result } = renderHook(() => useAgentChat())

    await act(async () => {
      result.current.setPlanMode(true)
      await result.current.send('请按计划执行')
    })

    const sendOptions = chatMock.sendMessage.mock.calls[0]?.[1]
    expect((sendOptions?.body as Record<string, unknown>)?.plan_mode).toBe(true)
  })

  it('ignores an older history response after a newer session history has loaded', async () => {
    const older = deferred<Awaited<ReturnType<typeof getMessagesPage>>>()
    const newer = deferred<Awaited<ReturnType<typeof getMessagesPage>>>()
    vi.mocked(getMessagesPage).mockImplementation((sessionId?: string) => sessionId === 'older' ? older.promise : newer.promise)
    const { result } = renderHook(() => useAgentChat())

    let olderRequest!: Promise<void>
    let newerRequest!: Promise<void>
    act(() => {
      olderRequest = result.current.loadHistory('older')
      newerRequest = result.current.loadHistory('newer')
    })

    await act(async () => {
      newer.resolve({ messages: [{ id: 'new-message', role: 'user', parts: [{ type: 'text', text: '新会话' }] }], nextBefore: '0', hasMore: false, total: 1 })
      await newerRequest
    })
    await act(async () => {
      older.resolve({ messages: [{ id: 'old-message', role: 'user', parts: [{ type: 'text', text: '旧会话' }] }], nextBefore: '0', hasMore: false, total: 1 })
      await olderRequest
    })

    expect(chatMock.setMessages).toHaveBeenCalledTimes(1)
    expect(chatMock.setMessages).toHaveBeenLastCalledWith([
      { id: 'new-message', role: 'user', parts: [{ type: 'text', text: '新会话' }] },
    ])
  })

  it('prepends an earlier history page without replacing the current live tail', async () => {
    vi.mocked(getMessagesPage)
      .mockResolvedValueOnce({
        messages: [{ id: 'message-2', role: 'assistant', parts: [{ type: 'text', text: '当前窗口' }] }],
        nextBefore: '1',
        hasMore: true,
        total: 2,
      })
      .mockResolvedValueOnce({
        messages: [{ id: 'message-1', role: 'user', parts: [{ type: 'text', text: '更早消息' }] }],
        nextBefore: '0',
        hasMore: false,
        total: 2,
      })
    const { result } = renderHook(() => useAgentChat())
    await act(async () => result.current.loadHistory('session-a'))
    chatMock.setMessages.mockClear()

    await act(async () => result.current.loadEarlierHistory())

    expect(getMessagesPage).toHaveBeenLastCalledWith('session-a', expect.objectContaining({ before: '1' }))
    const prepend = chatMock.setMessages.mock.calls[0]?.[0] as (messages: unknown[]) => unknown[]
    expect(prepend([
      { id: 'message-2', role: 'assistant', parts: [{ type: 'text', text: '当前窗口' }] },
      { id: 'live-message', role: 'assistant', parts: [{ type: 'text', text: '仍在流式输出', state: 'streaming' }] },
    ])).toEqual([
      { id: 'message-1', role: 'user', parts: [{ type: 'text', text: '更早消息' }] },
      { id: 'message-2', role: 'assistant', parts: [{ type: 'text', text: '当前窗口' }] },
      { id: 'live-message', role: 'assistant', parts: [{ type: 'text', text: '仍在流式输出', state: 'streaming' }] },
    ])
    expect(result.current.hasEarlierMessages).toBe(false)
  })

  it('startEditTurn 截断到源 user 消息并返回其原文（无 turn_id 写作模式）', async () => {
    chatMock.messages = [
      { id: 'm1', role: 'user', parts: [{ type: 'text', text: '第一轮输入' }], metadata: { message_index: 0 } },
      { id: 'm2', role: 'assistant', parts: [{ type: 'text', text: '第一轮回复' }], metadata: { message_index: 1 } },
      { id: 'm3', role: 'user', parts: [{ type: 'text', text: '第二轮输入' }], metadata: { message_index: 2 } },
      { id: 'm4', role: 'assistant', parts: [{ type: 'text', text: '第二轮回复' }], metadata: { message_index: 3 } },
    ]
    vi.mocked(switchSession).mockResolvedValue({ id: 's1', title: 's1', active: true, message_count: 4, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' })
    vi.mocked(getSessions).mockResolvedValue([{ id: 's1', title: 's1', active: true, message_count: 4, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' }])
    const { result } = renderHook(() => useAgentChat())
    await act(async () => { result.current.switchChatSession('s1') })

    const views = buildAgentMessageViews(chatMock.messages)
    const assistantView = views.find((v) => v.messageId === 'm4')!
    let editResult!: Promise<string | null>
    act(() => {
      editResult = result.current.startEditTurn(assistantView)
    })
    const content = await act(async () => editResult)

    // 截断到源 user 消息（m3，message_index=2），reason='edit'
    expect(truncateSession).toHaveBeenCalledWith('s1', 2, 'edit')
    // 返回源 user 消息原文供 composer 预填
    expect(content).toBe('第二轮输入')
    // 本地 UI 截断：保留到 m3（含），移除 m4
    const updater = chatMock.setMessages.mock.calls.map((c) => c[0]).find((a) => typeof a === 'function')
    expect(updater).toBeDefined()
    expect((updater as (all: any[]) => any[])(chatMock.messages).map((m: any) => m.id)).toEqual(['m1', 'm2', 'm3'])
  })

  it('startEditTurn 对 user 消息本身编辑时以该消息为源（不取上一条 user）', async () => {
    chatMock.messages = [
      { id: 'm1', role: 'user', parts: [{ type: 'text', text: '第一轮输入' }], metadata: { message_index: 0 } },
      { id: 'm2', role: 'assistant', parts: [{ type: 'text', text: '第一轮回复' }], metadata: { message_index: 1 } },
      { id: 'm3', role: 'user', parts: [{ type: 'text', text: '第二轮输入' }], metadata: { message_index: 2 } },
    ]
    vi.mocked(switchSession).mockResolvedValue({ id: 's1', title: 's1', active: true, message_count: 3, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' })
    vi.mocked(getSessions).mockResolvedValue([{ id: 's1', title: 's1', active: true, message_count: 3, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' }])
    const { result } = renderHook(() => useAgentChat())
    await act(async () => { result.current.switchChatSession('s1') })

    const views = buildAgentMessageViews(chatMock.messages)
    const userView = views.find((v) => v.messageId === 'm3')!
    let editResult!: Promise<string | null>
    act(() => {
      editResult = result.current.startEditTurn(userView)
    })
    const content = await act(async () => editResult)

    // user 消息本身就是源：截断到 m3（message_index=2），返回 m3 原文（而非 m1）
    expect(truncateSession).toHaveBeenCalledWith('s1', 2, 'edit')
    expect(content).toBe('第二轮输入')
  })

  it('regenerateMessage 对历史加载消息重试：截断并重发源 user 原文', async () => {
    chatMock.messages = [
      { id: 'm1', role: 'user', parts: [{ type: 'text', text: '历史输入' }], metadata: { message_index: 0 } },
      { id: 'm2', role: 'assistant', parts: [{ type: 'text', text: '历史回复' }], metadata: { message_index: 1 } },
    ]
    chatMock.sendMessage.mockResolvedValue(undefined)
    vi.mocked(switchSession).mockResolvedValue({ id: 's1', title: 's1', active: true, message_count: 2, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' })
    vi.mocked(getSessions).mockResolvedValue([{ id: 's1', title: 's1', active: true, message_count: 2, created_at: '2026-08-08T00:00:00Z', updated_at: '2026-08-08T00:00:00Z' }])
    const { result } = renderHook(() => useAgentChat())
    await act(async () => { result.current.switchChatSession('s1') })

    const views = buildAgentMessageViews(chatMock.messages)
    const assistantView = views.find((v) => v.messageId === 'm2')!
    let regenResult!: Promise<boolean>
    act(() => {
      regenResult = result.current.regenerateMessage(assistantView)
    })
    const ok = await act(async () => regenResult)

    expect(ok).toBe(true)
    // 截断到源 user 消息（m1，message_index=0），reason='retry'
    expect(truncateSession).toHaveBeenCalledWith('s1', 0, 'retry')
    // 重发源 user 原文
    expect(chatMock.sendMessage).toHaveBeenCalledWith(
      expect.objectContaining({ parts: [{ type: 'text', text: '历史输入' }] }),
      expect.any(Object),
    )
  })
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}
