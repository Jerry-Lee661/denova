package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCacheHitRateIncreasesWithRepeatedPrefix 验证前缀稳定 + KV cache 命中可复现基线：
// 给定一条 trace，其中同一前缀被重复请求，cached_prompt_tokens 应随轮次递增，
// 最终 cache_hit_rate 趋近稳定值（反映前缀命中缓存）。这是任务 6 观测结果的
// 可复现固化——不依赖服务端 /metrics，仅用 Denova 自身 run_trace 即可断言。
func TestCacheHitRateIncreasesWithRepeatedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-prefix-stable.jsonl")
	content := strings.Join([]string{
		`{"type":"run_created","run_id":"run-prefix","created_at":"2026-07-09T00:00:00Z","data":{"agent_kind":"ide"}}`,
		// 第 1 轮：前缀 1000 token，全部未命中缓存。
		`{"type":"llm_call","run_id":"run-prefix","created_at":"2026-07-09T00:00:01Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":0,"uncached_prompt_tokens":1000,"total_tokens":1200}}}`,
		// 第 2 轮：前缀命中 500，剩余 500 新增。
		`{"type":"llm_call","run_id":"run-prefix","created_at":"2026-07-09T00:00:02Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":500,"uncached_prompt_tokens":500,"total_tokens":1300}}}`,
		// 第 3 轮：前缀命中 800，剩余 200 新增。
		`{"type":"llm_call","run_id":"run-prefix","created_at":"2026-07-09T00:00:03Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":800,"uncached_prompt_tokens":200,"total_tokens":1400}}}`,
		// 第 4 轮：前缀命中 950，剩余 50 新增（趋近稳定）。
		`{"type":"llm_call","run_id":"run-prefix","created_at":"2026-07-09T00:00:04Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":950,"uncached_prompt_tokens":50,"total_tokens":1450}}}`,
		`{"type":"run_finished","run_id":"run-prefix","created_at":"2026-07-09T00:00:05Z","data":{"status":"success"}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	trace, err := readRunTraceFile(path, defaultRunTraceRecordCap)
	if err != nil {
		t.Fatal(err)
	}

	// 4 次 LLM 调用。
	if trace.Summary.LLMCalls != 4 {
		t.Fatalf("llm calls = %d, want 4", trace.Summary.LLMCalls)
	}

	// 累计 cached = 0+500+800+950 = 2250；累计 prompt = 4000。
	if trace.Summary.CachedPromptTokens != 2250 {
		t.Fatalf("cached prompt tokens = %d, want 2250", trace.Summary.CachedPromptTokens)
	}
	if trace.Summary.PromptTokens != 4000 {
		t.Fatalf("prompt tokens = %d, want 4000", trace.Summary.PromptTokens)
	}

	// cache_hit_rate = 2250/4000 = 0.5625，显著高于首轮 0（反映前缀命中缓存）。
	if trace.Summary.CacheHitRate <= 0 {
		t.Fatalf("cache hit rate = %.4f, want > 0 (prefix stable)", trace.Summary.CacheHitRate)
	}
	if trace.Summary.CacheHitRate < 0.5 {
		t.Fatalf("cache hit rate = %.4f, want >= 0.5 for stable prefix", trace.Summary.CacheHitRate)
	}
}

// TestCacheHitRateStableWhenFullyCached 验证全部前缀命中时 cache_hit_rate 趋近 1。
func TestCacheHitRateStableWhenFullyCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-fully-cached.jsonl")
	content := strings.Join([]string{
		`{"type":"run_created","run_id":"run-full","created_at":"2026-07-09T00:00:00Z","data":{"agent_kind":"ide"}}`,
		`{"type":"llm_call","run_id":"run-full","created_at":"2026-07-09T00:00:01Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":1000,"uncached_prompt_tokens":0,"total_tokens":1200}}}`,
		`{"type":"llm_call","run_id":"run-full","created_at":"2026-07-09T00:00:02Z","data":{"attrs":{"prompt_tokens":1000,"cached_prompt_tokens":1000,"uncached_prompt_tokens":0,"total_tokens":1200}}}`,
		`{"type":"run_finished","run_id":"run-full","created_at":"2026-07-09T00:00:03Z","data":{"status":"success"}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	trace, err := readRunTraceFile(path, defaultRunTraceRecordCap)
	if err != nil {
		t.Fatal(err)
	}

	if trace.Summary.CacheHitRate < 0.99 {
		t.Fatalf("cache hit rate = %.4f, want ~1.0 when fully cached", trace.Summary.CacheHitRate)
	}
}
