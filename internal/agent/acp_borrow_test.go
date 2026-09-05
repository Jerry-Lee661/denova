package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/session"
)

// ── T1：模型自主压缩工具 ──────────────────────────────────────────────

// fakeContextToolConversation 是测试用的会话桩，同时实现
// ContextCompactionConversation（供 contextWithCompactionController 识别）
// 与 ContextToolConversation（供三个工具调用）。
type fakeContextToolConversation struct {
	compressCalls  []ContextToolCompressInput
	searchCalls    []string
	statusCalls    int
	compressResult ContextToolCompressResult
}

func (f *fakeContextToolConversation) CompactContextIfNeeded(_ context.Context, _ ContextCompactionInput) ([]*schema.Message, ContextCompactionResult, error) {
	return nil, ContextCompactionResult{}, nil
}

func (f *fakeContextToolConversation) CompressContextRange(_ context.Context, input ContextToolCompressInput) (ContextToolCompressResult, error) {
	f.compressCalls = append(f.compressCalls, input)
	return f.compressResult, nil
}

func (f *fakeContextToolConversation) SearchContextHistory(query string) []contextToolSearchHit {
	f.searchCalls = append(f.searchCalls, query)
	return []contextToolSearchHit{{Index: 3, Role: "user", Snippet: "命中片段"}}
}

func (f *fakeContextToolConversation) ContextToolStatus(_ []*schema.ToolInfo) contextToolStatusResult {
	f.statusCalls++
	return contextToolStatusResult{Schema: "acp_status.v1", EstimatedTokens: 100, ContextWindowTokens: 1000, UsageRatio: 0.1, RawMessageCount: 5}
}

// fakeConvContext 直接构造带 compaction controller 的 context（同包可访问未导出标识），
// 避免 fake 实现完整 Conversation 接口。
func fakeConvContext(conv ContextCompactionConversation) context.Context {
	return context.WithValue(context.Background(), contextCompactionContextKey{}, &contextCompactionController{conversation: conv})
}

func contextToolByName(t *testing.T, tools []tool.BaseTool, name string) tool.InvokableTool {
	t.Helper()
	for _, item := range tools {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == name {
			invokable, ok := item.(tool.InvokableTool)
			if !ok {
				t.Fatalf("tool %s not invokable: %T", name, item)
			}
			return invokable
		}
	}
	t.Fatalf("tool %s not registered", name)
	return nil
}

func TestNewContextToolsRegistersTwoTools(t *testing.T) {
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	for _, name := range []string{contextToolNameCompress, contextToolNameAcpContext} {
		_ = contextToolByName(t, tools, name)
	}
}

func TestContextToolCompressFoldAndCompact(t *testing.T) {
	conv := &fakeContextToolConversation{compressResult: ContextToolCompressResult{Triggered: true, Summary: "摘要", TokensBefore: 500, TokensAfter: 50}}
	ctx := fakeConvContext(conv)
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	compress := contextToolByName(t, tools, contextToolNameCompress)

	// fold（默认）
	out, err := compress.InvokableRun(ctx, `{"start":1,"end":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"mode":"fold"`) || !strings.Contains(out, `"triggered":true`) {
		t.Fatalf("unexpected fold output: %s", out)
	}
	if len(conv.compressCalls) != 1 || conv.compressCalls[0].Mode != contextToolModeFold {
		t.Fatalf("fold mode not propagated: %#v", conv.compressCalls)
	}

	// compact
	if _, err := compress.InvokableRun(ctx, `{"start":1,"end":3,"mode":"compact"}`); err != nil {
		t.Fatal(err)
	}
	if len(conv.compressCalls) != 2 || conv.compressCalls[1].Mode != contextToolModeCompact {
		t.Fatalf("compact mode not propagated: %#v", conv.compressCalls)
	}

	// 非法 mode
	if _, err := compress.InvokableRun(ctx, `{"start":1,"end":3,"mode":"bogus"}`); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestContextToolCompressRequiresConversation(t *testing.T) {
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	compress := contextToolByName(t, tools, contextToolNameCompress)
	if _, err := compress.InvokableRun(context.Background(), `{"start":1,"end":3}`); err == nil {
		t.Fatal("expected error when no conversation in context")
	}
}

func TestContextToolSearchContext(t *testing.T) {
	conv := &fakeContextToolConversation{}
	ctx := fakeConvContext(conv)
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	acp := contextToolByName(t, tools, contextToolNameAcpContext)

	out, err := acp.InvokableRun(ctx, `{"op":"search_context","args":{"query":"关键词"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"total":1`) || !strings.Contains(out, "命中片段") {
		t.Fatalf("unexpected search output: %s", out)
	}
	if len(conv.searchCalls) != 1 || conv.searchCalls[0] != "关键词" {
		t.Fatalf("query not propagated: %#v", conv.searchCalls)
	}
	// 空 query 报错
	if _, err := acp.InvokableRun(ctx, `{"op":"search_context","args":{"query":"  "}}`); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestContextToolAcpStatus(t *testing.T) {
	conv := &fakeContextToolConversation{}
	ctx := fakeConvContext(conv)
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	acp := contextToolByName(t, tools, contextToolNameAcpContext)

	out, err := acp.InvokableRun(ctx, `{"op":"acp_status","args":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"schema":"acp_status.v1"`) || !strings.Contains(out, `"usage_ratio":0.1`) {
		t.Fatalf("unexpected status output: %s", out)
	}
	if conv.statusCalls != 1 {
		t.Fatalf("status not called once: %d", conv.statusCalls)
	}
}

func TestContextToolAcpContextHelp(t *testing.T) {
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	acp := contextToolByName(t, tools, contextToolNameAcpContext)

	out, err := acp.InvokableRun(context.Background(), `{"op":"help"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "search_context") || !strings.Contains(out, "acp_status") {
		t.Fatalf("help output missing ops: %s", out)
	}
}

func TestContextToolAcpContextUnknownOp(t *testing.T) {
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	acp := contextToolByName(t, tools, contextToolNameAcpContext)

	_, err := acp.InvokableRun(context.Background(), `{"op":"bogus"}`)
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("expected unknown op error, got: %v", err)
	}
}

func TestContextToolAcpContextSearchRequiresQuery(t *testing.T) {
	tools := NewContextTools(&config.Config{}, config.AgentKindIDE)
	acp := contextToolByName(t, tools, contextToolNameAcpContext)

	_, err := acp.InvokableRun(context.Background(), `{"op":"search_context","args":{}}`)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("expected query-is-required error, got: %v", err)
	}
}

// ── T2：三层分级压缩 ──────────────────────────────────────────────────

func TestTieredNextTierPriority(t *testing.T) {
	th := contextTieredThresholds{T1: 24000, T2: 8000, T3: 3000}

	// T1 优先：RawTokens 超 T1 阈值 → 层 1
	tier, reason := tieredNextTier(session.ContextTieredPool{RawTokens: 25000, T1Tokens: 9000, T2Tokens: 4000}, th)
	if tier != 1 || reason != "raw_pool_over_t1" {
		t.Fatalf("T1 priority failed: tier=%d reason=%s", tier, reason)
	}
	// T2：T1Tokens 超 T2 阈值 → 层 2
	tier, reason = tieredNextTier(session.ContextTieredPool{RawTokens: 100, T1Tokens: 9000, T2Tokens: 4000}, th)
	if tier != 2 || reason != "t1_pool_over_t2" {
		t.Fatalf("T2 failed: tier=%d reason=%s", tier, reason)
	}
	// T3：T2Tokens 超 T3 阈值 → 层 3
	tier, reason = tieredNextTier(session.ContextTieredPool{RawTokens: 100, T1Tokens: 100, T2Tokens: 4000}, th)
	if tier != 3 || reason != "t2_pool_over_t3" {
		t.Fatalf("T3 failed: tier=%d reason=%s", tier, reason)
	}
	// 全低于阈值
	tier, reason = tieredNextTier(session.ContextTieredPool{RawTokens: 10, T1Tokens: 10, T2Tokens: 10}, th)
	if tier != 0 || reason != "below_threshold" {
		t.Fatalf("below threshold failed: tier=%d reason=%s", tier, reason)
	}
}

func TestTieredProjectionSummaryUsesHighestTier(t *testing.T) {
	// 只有 T1
	if got := tieredProjectionSummary(session.ContextTieredPool{T1Summary: "T1摘要"}); got != "T1摘要" {
		t.Fatalf("T1 projection = %q", got)
	}
	// T2 优先于 T1
	if got := tieredProjectionSummary(session.ContextTieredPool{T1Summary: "T1摘要", T2Summary: "T2摘要"}); got != "T2摘要" {
		t.Fatalf("T2 projection = %q", got)
	}
	// T3 优先于 T2/T1
	if got := tieredProjectionSummary(session.ContextTieredPool{T1Summary: "T1", T2Summary: "T2", T3Summary: "T3摘要"}); got != "T3摘要" {
		t.Fatalf("T3 projection = %q", got)
	}
	// 空池
	if got := tieredProjectionSummary(session.ContextTieredPool{}); got != "" {
		t.Fatalf("empty projection = %q", got)
	}
}

func TestRunTieredDistillProgression(t *testing.T) {
	previous := summarizeContextForCompaction
	defer func() { summarizeContextForCompaction = previous }()

	// 桩：返回固定摘要，便于断言层级递进。
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, _ string, source []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		// source[0] 是被蒸馏的上一层摘要。
		return "蒸馏自: " + source[0].Content, 10, nil
	}

	th := contextTieredThresholds{T1: 100, T2: 50, T3: 20}
	policy := contextCompactionPolicy{}

	// 初始：T1 已捕获（T1Tokens 超 T2、T2 蒸馏后超 T3 的级联）。
	pool := session.ContextTieredPool{T1Summary: "原始详细摘要", T1Tokens: 60}
	pool, err := runTieredDistill(context.Background(), &config.Config{}, config.AgentKindIDE, pool, th, policy)
	if err != nil {
		t.Fatal(err)
	}
	// T2 蒸馏：T1Tokens(60) >= T2(50) → 生成 T2，T1Tokens 清零。
	if pool.T2Summary == "" || pool.T1Tokens != 0 {
		t.Fatalf("T2 distill failed: %#v", pool)
	}
	// T3 蒸馏：T2Tokens(10) < T3(20) → 不触发 T3。
	if pool.T3Summary != "" {
		t.Fatalf("T3 should not trigger: %#v", pool)
	}
	// 投影使用最高可用层 T2。
	if got := tieredProjectionSummary(pool); got != pool.T2Summary {
		t.Fatalf("projection should be T2: %q", got)
	}
}

func TestTieredPoolActive(t *testing.T) {
	if !tieredPoolActive(session.ContextTieredPool{T1Summary: "x"}) {
		t.Fatal("T1 active should be true")
	}
	if !tieredPoolActive(session.ContextTieredPool{T3Summary: "x"}) {
		t.Fatal("T3 active should be true")
	}
	if tieredPoolActive(session.ContextTieredPool{}) {
		t.Fatal("empty pool should be inactive")
	}
}

// ── T3：压缩质量门 ────────────────────────────────────────────────────

func TestCheckCompactionQualityL1TooShort(t *testing.T) {
	original := strings.Repeat("这是一段很长的原始对话内容，包含大量细节和上下文信息。", 20)
	summary := "太短"
	v := CheckCompactionQuality(original, summary)
	if v.L1Passed {
		t.Fatalf("L1 should fail for short summary: %#v", v)
	}
	if v.Passed {
		t.Fatalf("overall should fail: %#v", v)
	}
}

func TestCheckCompactionQualityL1Retention(t *testing.T) {
	// 原文 10 万字符，1% = 1000 字符；摘要 500 字符（>200，长度检查通过）
	// 但保留率 0.5% < 1% → 保留率检查失败。
	original := strings.Repeat("原始内容填充。", 16667) // ~100002 字符
	summary := strings.Repeat("摘", 500)
	v := CheckCompactionQuality(original, summary)
	if v.SummaryChars < compactionQualityMinSummaryChars {
		t.Fatalf("setup: summary should exceed min chars, got %d", v.SummaryChars)
	}
	if v.L1Passed {
		t.Fatalf("L1 should fail for low retention: retention=%.4f %#v", v.RetentionRatio, v)
	}
}

func TestCheckCompactionQualityPass(t *testing.T) {
	original := "用户要求实现三层压缩功能，包含 T1 捕获、T2 蒸馏、T3 浓缩。子代理审查了代码，冗长命令输出被折叠，死路探索被标记。"
	summary := "用户要求实现三层压缩功能，包含 T1 捕获、T2 蒸馏、T3 浓缩三个层级。子代理审查了代码实现，冗长命令输出被折叠为摘要占位符，死路探索被明确标记。保留用户意图与任务约束，未闭环事项包括层级阈值配置与质量门开关。事件时间顺序完整，因果来源已标注，长期影响信息均已保留，供后续模型继续推进压缩功能开发与验证工作。压缩优先级策略已注入系统提示词，尾部偏置折叠引导保持稳定前缀以命中缓存。三层摘要池持久化于压缩记录，投影使用最高可用层，保证投影恒定不每轮重新压缩。"
	v := CheckCompactionQuality(original, summary)
	if !v.Passed {
		t.Fatalf("should pass: %#v", v)
	}
}

func TestCheckCompactionQualityL2Coverage(t *testing.T) {
	// 原文关键词丰富，摘要完全无关 → L2 失败。
	original := "量子纠缠 拓扑绝缘体 超导 磁通量 能带结构 费米子 玻色子 自旋 轨道耦合 晶格振动 声子 等离子体 磁约束 惯性约束 激光聚变"
	summary := "今天天气不错，我们出去散步，顺便买了一些水果和蔬菜，回家做了一顿丰盛的晚餐，大家吃得都很开心。"
	v := CheckCompactionQuality(original, summary)
	if v.L2Passed {
		t.Fatalf("L2 should fail for low coverage: f1=%.4f recall=%.4f %#v", v.UnigramF1, v.KeywordRecall, v)
	}
}

// ── T4：压缩候选优先级排序 ────────────────────────────────────────────

func TestRankCompactionCandidatesPriorityOrder(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("用户消息"),
		schema.AssistantMessage("子代理审查结论：代码通过", nil),
		schema.AssistantMessage("这是一条死路探索，此路不通", nil),
		schema.AssistantMessage("步骤 1：中间产物", nil),
		schema.AssistantMessage("已解决讨论线程", nil),
	}
	// 冗长命令输出（tool 角色，长内容）
	verbose := schema.Message{Role: schema.Tool, Content: strings.Repeat("command output line\n", 200)}
	messages = append(messages, &verbose)

	candidates := rankCompactionCandidates(messages)
	if len(candidates) != len(messages) {
		t.Fatalf("candidate count = %d, want %d", len(candidates), len(messages))
	}
	// 第一个候选应是子代理审查（最高优先级）。
	if candidates[0].Priority != compactionPrioritySubAgentReview {
		t.Fatalf("first candidate should be subagent review, got priority %d (index %d)", candidates[0].Priority, candidates[0].Index)
	}
	// 冗长命令输出（index 5）应排在死路探索（index 2）之前。
	verboseIdx := -1
	deadEndIdx := -1
	for i, c := range candidates {
		if c.Index == 5 {
			verboseIdx = i
		}
		if c.Index == 2 {
			deadEndIdx = i
		}
	}
	if verboseIdx == -1 || deadEndIdx == -1 || verboseIdx > deadEndIdx {
		t.Fatalf("verbose command (pos %d) should rank before dead-end (pos %d)", verboseIdx, deadEndIdx)
	}
}

func TestClassifyCompactionCandidateCategories(t *testing.T) {
	cases := []struct {
		name string
		msg  *schema.Message
		want compactionCandidatePriority
	}{
		{"subagent", schema.AssistantMessage("subagent review 结论", nil), compactionPrioritySubAgentReview},
		{"dead_end", schema.AssistantMessage("这是 dead end 死路", nil), compactionPriorityDeadEnd},
		{"multi_step", schema.AssistantMessage("step 2 intermediate", nil), compactionPriorityMultiStepIntermediate},
		{"resolved", schema.AssistantMessage("resolved 已解决", nil), compactionPriorityResolvedDiscussion},
		{"verbose_tool", &schema.Message{Role: schema.Tool, Content: strings.Repeat("x", 4000)}, compactionPriorityVerboseCommand},
		{"redundant_tool", &schema.Message{Role: schema.Tool, Content: "ok"}, compactionPriorityRedundantToolResult},
		{"large_file", schema.UserMessage(strings.Repeat("大文件内容", 500)), compactionPriorityLargeFileUsed},
		{"default_user", schema.UserMessage("普通用户消息"), compactionPriorityDefault},
	}
	for _, tc := range cases {
		if got := classifyCompactionCandidate(tc.msg); got != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCompactionPriorityGuidanceInSystemPrompt(t *testing.T) {
	prompt := contextCompactionSystemInstruction()
	if !strings.Contains(prompt, "压缩优先级") || !strings.Contains(prompt, "尾部偏置") {
		t.Fatalf("system prompt missing priority/tail-biased guidance")
	}
}

// ── T-B1/T-B2：TTFT 滑动窗口与预热压缩 ────────────────────────────────

// fakePreheatConversation 是 shouldPreheatCompaction off/on 分支用的最小
// Conversation 桩（off/on 分支不触碰会话内部，无需 *SessionConversation）。
type fakePreheatConversation struct{}

func (fakePreheatConversation) PrepareMessages(_, _ string) ([]*schema.Message, error) {
	return nil, nil
}
func (fakePreheatConversation) AppendAssistant(_ string) error             { return nil }
func (fakePreheatConversation) MarkInterrupted(_, _, _ string) error       { return nil }
func (fakePreheatConversation) PendingInterruption() *session.Interruption { return nil }
func (fakePreheatConversation) ResolveInterruption(_ string) error         { return nil }
func (fakePreheatConversation) RuntimeState() session.RuntimeState         { return session.RuntimeState{} }

func preheatTestConfig(mode string) *config.Config {
	cfg := &config.Config{}
	cfg.AgentContexts.Default.PreheatCompactionEnabled = &mode
	return cfg
}

func TestRecordAndAverageTTFT(t *testing.T) {
	resetTTFTStatsForTest()
	defer resetTTFTStatsForTest()

	base := "http://127.0.0.1:8191/v1"
	recordTTFT(base, 100)
	recordTTFT(base, 200)
	recordTTFT(base, 300)
	avg, ok := averageTTFT(base)
	if !ok {
		t.Fatal("expected sample present")
	}
	if avg != 200 {
		t.Fatalf("average = %v, want 200", avg)
	}
	resetTTFTStatsForTest()
	if _, ok := averageTTFT(base); ok {
		t.Fatal("expected no sample after reset")
	}
}

func TestTTFTSlidingWindow(t *testing.T) {
	resetTTFTStatsForTest()
	defer resetTTFTStatsForTest()

	base := "http://127.0.0.1:8187/v1"
	for i := 1; i <= 10; i++ {
		recordTTFT(base, float64(i*100))
	}
	avg, ok := averageTTFT(base)
	if !ok {
		t.Fatal("expected sample present")
	}
	// 窗口只保留最近 8 个样本（300..1000），均值应为 650 而非全部 10 个的 550。
	if avg != 650 {
		t.Fatalf("sliding window average = %v, want 650", avg)
	}
}

func TestIsLocalModelBaseURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8191/v1", true},
		{"http://localhost:8187/v1", true},
		{"https://api.openai.com/v1", false},
	}
	for _, tc := range cases {
		if got := isLocalModelBaseURL(tc.url); got != tc.want {
			t.Fatalf("isLocalModelBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestShouldPreheatCompactionOff(t *testing.T) {
	if shouldPreheatCompaction(context.Background(), preheatTestConfig("off"), config.AgentKindIDE, fakePreheatConversation{}) {
		t.Fatal("off should never preheat")
	}
}

func TestShouldPreheatCompactionOn(t *testing.T) {
	if !shouldPreheatCompaction(context.Background(), preheatTestConfig("on"), config.AgentKindIDE, fakePreheatConversation{}) {
		t.Fatal("on should always preheat")
	}
}

func TestShouldPreheatCompactionAutoFarFromTrigger(t *testing.T) {
	// auto 为默认值。fake 不是 *SessionConversation，在类型断言门即返回 false；
	// 真实的「已用 token < 0.8×trigger」路径依赖 SessionConversation 内部状态，留给集成验证。
	if shouldPreheatCompaction(context.Background(), &config.Config{}, config.AgentKindIDE, fakePreheatConversation{}) {
		t.Fatal("auto with non-SessionConversation should not preheat")
	}
}
