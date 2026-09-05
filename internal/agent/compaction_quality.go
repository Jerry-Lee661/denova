package agent

import (
	"log/slog"
	"strings"
	"unicode"
)

// 压缩质量门（Plan.md §12 / T3，借鉴 ACP rouge-recall-v1，clean-room 简易实现）。
//
// 在摘要生成后校验摘要质量，失败只 log.Warn 不阻断（避免质量门误杀正常压缩）。
// 两级检查：
//   - L1 长度下限：摘要 <200 字符 或 保留率 <1% → 摘要过短，可能丢失关键信息。
//   - L2 内容覆盖：ROUGE-1 风格 unigram F1 <0.05 且 top-20 关键词召回 <0.20（AND）
//     → 摘要与原文内容覆盖不足。
//
// 默认关闭，由 [agent] compaction_quality_gate_enabled 开关控制。

const (
	compactionQualityMinSummaryChars  = 200
	compactionQualityMinRetention     = 0.01
	compactionQualityMinUnigramF1     = 0.05
	compactionQualityMinKeywordRecall = 0.20
	compactionQualityTopKeywords      = 20
)

// CompactionQualityVerdict 是一次质量门检查的结论。
type CompactionQualityVerdict struct {
	Passed         bool     `json:"passed"`
	L1Passed       bool     `json:"l1_passed"`
	L2Passed       bool     `json:"l2_passed"`
	SummaryChars   int      `json:"summary_chars"`
	RetentionRatio float64  `json:"retention_ratio"`
	UnigramF1      float64  `json:"unigram_f1"`
	KeywordRecall  float64  `json:"keyword_recall"`
	Reasons        []string `json:"reasons,omitempty"`
}

// CheckCompactionQuality 校验摘要质量（纯函数，便于单测）。
// original 是被压缩的原文文本，summary 是生成的摘要。
func CheckCompactionQuality(original, summary string) CompactionQualityVerdict {
	verdict := CompactionQualityVerdict{
		Passed:   true,
		L1Passed: true,
		L2Passed: true,
		Reasons:  make([]string, 0, 2),
	}
	summary = strings.TrimSpace(summary)
	original = strings.TrimSpace(original)
	summaryChars := countRunes(summary)
	originalChars := countRunes(original)
	verdict.SummaryChars = summaryChars

	// L1 长度下限
	retention := 0.0
	if originalChars > 0 {
		retention = float64(summaryChars) / float64(originalChars)
	}
	verdict.RetentionRatio = retention
	if summaryChars < compactionQualityMinSummaryChars {
		verdict.L1Passed = false
		verdict.Reasons = append(verdict.Reasons, "summary_too_short")
	}
	if retention < compactionQualityMinRetention {
		verdict.L1Passed = false
		verdict.Reasons = append(verdict.Reasons, "retention_below_1pct")
	}

	// L2 内容覆盖
	f1 := unigramF1(original, summary)
	recall := keywordRecall(original, summary, compactionQualityTopKeywords)
	verdict.UnigramF1 = f1
	verdict.KeywordRecall = recall
	if f1 < compactionQualityMinUnigramF1 && recall < compactionQualityMinKeywordRecall {
		verdict.L2Passed = false
		verdict.Reasons = append(verdict.Reasons, "content_coverage_low")
	}

	verdict.Passed = verdict.L1Passed && verdict.L2Passed
	return verdict
}

// logCompactionQuality 在质量门失败时打 Warn 日志（非阻断）。
// 调用方负责按配置开关决定是否调用。
func logCompactionQuality(agentKind, phase string, original, summary string) {
	verdict := CheckCompactionQuality(original, summary)
	if verdict.Passed {
		return
	}
	slog.Warn("compaction_quality_gate",
		slog.String("agent_kind", agentKind),
		slog.String("phase", phase),
		slog.Bool("l1_passed", verdict.L1Passed),
		slog.Bool("l2_passed", verdict.L2Passed),
		slog.Int("summary_chars", verdict.SummaryChars),
		slog.Float64("retention_ratio", verdict.RetentionRatio),
		slog.Float64("unigram_f1", verdict.UnigramF1),
		slog.Float64("keyword_recall", verdict.KeywordRecall),
		slog.String("reasons", strings.Join(verdict.Reasons, ",")),
	)
}

// tokenize 把文本切成小写词元（按非字母数字边界切分，保留 CJK 单字）。
func tokenize(text string) []string {
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, string(current))
			current = current[:0]
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// CJK 字符单独成词（无空格分词），其余字母数字累积成词。
			if isCJK(r) {
				flush()
				tokens = append(tokens, string(r))
			} else {
				current = append(current, unicode.ToLower(r))
			}
		default:
			flush()
		}
	}
	flush()
	return tokens
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意文字
		(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
		(r >= 0x3040 && r <= 0x30FF) || // 日文平假名/片假名
		(r >= 0xAC00 && r <= 0xD7AF) // 韩文
}

// compactionQualityStopwords 是极简停用词表（clean-room，仅高频虚词）。
var compactionQualityStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"in": true, "on": true, "to": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "for": true, "with": true, "as": true,
	"it": true, "this": true, "that": true, "these": true, "those": true,
	"我们": true, "你们": true, "他们": true, "这个": true, "那个": true, "一个": true,
}

// topKeywords 返回文本中频率最高的 n 个关键词（去停用词）。
func topKeywords(text string, n int) []string {
	freq := make(map[string]int)
	for _, tok := range tokenize(text) {
		if len(tok) < 2 {
			// 单字符词元仅当为 CJK 单字时保留（isCJKRune 接受 rune）。
			runes := []rune(tok)
			if len(runes) != 1 || !isCJKRune(runes[0]) {
				continue
			}
		}
		if compactionQualityStopwords[tok] {
			continue
		}
		freq[tok]++
	}
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(freq))
	for k, v := range freq {
		items = append(items, kv{k, v})
	}
	// 简单选择排序取 top-n（n 很小，无需引入 sort 包）。
	for i := 0; i < n && i < len(items); i++ {
		maxIdx := i
		for j := i + 1; j < len(items); j++ {
			if items[j].v > items[maxIdx].v {
				maxIdx = j
			}
		}
		items[i], items[maxIdx] = items[maxIdx], items[i]
	}
	result := make([]string, 0, n)
	for i := 0; i < n && i < len(items); i++ {
		result = append(result, items[i].k)
	}
	return result
}

// keywordRecall 计算原文 top-n 关键词在摘要中的召回率。
func keywordRecall(original, summary string, n int) float64 {
	keywords := topKeywords(original, n)
	if len(keywords) == 0 {
		return 1.0
	}
	summaryLower := strings.ToLower(summary)
	hit := 0
	for _, kw := range keywords {
		if strings.Contains(summaryLower, strings.ToLower(kw)) {
			hit++
		}
	}
	return float64(hit) / float64(len(keywords))
}

// unigramF1 计算 ROUGE-1 风格的 unigram F1（原文 vs 摘要）。
func unigramF1(original, summary string) float64 {
	origFreq := make(map[string]int)
	for _, tok := range tokenize(original) {
		origFreq[tok]++
	}
	sumFreq := make(map[string]int)
	for _, tok := range tokenize(summary) {
		sumFreq[tok]++
	}
	// 精确匹配数（按摘要词元，受原文频率上限约束）
	matches := 0
	sumLen := 0
	for tok, cnt := range sumFreq {
		sumLen += cnt
		if origFreq[tok] > 0 {
			m := cnt
			if origFreq[tok] < m {
				m = origFreq[tok]
			}
			matches += m
		}
	}
	origLen := 0
	for _, cnt := range origFreq {
		origLen += cnt
	}
	if sumLen == 0 || origLen == 0 {
		return 0.0
	}
	precision := float64(matches) / float64(sumLen)
	recall := float64(matches) / float64(origLen)
	if precision+recall == 0 {
		return 0.0
	}
	return 2 * precision * recall / (precision + recall)
}
