package agent

import (
	"context"
	"log/slog"
	"net/url"

	"denova/config"
	"denova/internal/keyedlock"
)

// preheatCompactionNearTriggerRatio is the fraction of the compaction trigger
// line at which adaptive preheat starts considering the context "close enough"
// to the trigger (T-B2).
const preheatCompactionNearTriggerRatio = 0.8

// preheatCompactionSlowTTFTMs is the average first-chunk latency (ms) above
// which the model is considered slow enough that a background pre-fold is
// worth its cost (T-B2).
const preheatCompactionSlowTTFTMs = 10000

// preheatCompactionMinIncrementTokens is the minimum estimated token count of
// the pending incremental source below which folding would save too little to
// justify the background LLM call (T-B2).
const preheatCompactionMinIncrementTokens = 10000

// preheatSessionLocks serializes preheat folds per session so two consecutive
// runs never fold the same incremental source concurrently.
var preheatSessionLocks = keyedlock.New(nil)

// shouldPreheatCompaction decides whether a background pre-fold should run
// after a successful run (T-B2). The flag comes from
// ResolvedAgentContextSettings.PreheatCompactionEnabled: "off" never preheats,
// "on" always preheats, and "auto" (default) requires all three runtime
// signals: the context is near the compaction trigger line, compaction is
// expensive (slow TTFT or local model), and the pending incremental source is
// large enough to be worth folding.
func shouldPreheatCompaction(ctx context.Context, cfg *config.Config, agentKind string, conversation Conversation) bool {
	if cfg == nil || conversation == nil {
		return false
	}
	settings := config.ResolveAgentContext(cfg, agentKind)
	switch settings.PreheatCompactionEnabled {
	case "off":
		return false
	case "on":
		return true
	}
	// auto: derive from runtime facts.
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil || sc.session == nil {
		return false
	}
	// a) Near the trigger line: estimated used tokens >= 0.8 * triggerTokens().
	policy := resolveContextCompactionPolicy(cfg, agentKind)
	trigger := policy.triggerTokens()
	if trigger <= 0 {
		return false
	}
	usedTokens := EstimateContextTokens(sc.session.GetEffectiveMessages(), nil)
	if usedTokens < int(float64(trigger)*preheatCompactionNearTriggerRatio) {
		return false
	}
	// b) Compaction is expensive: average TTFT above the slow threshold, or a
	// local model (loopback host) where prefill cost is paid on the user's
	// machine.
	baseURL := config.ResolveAgentModel(cfg, agentKind).OpenAIBaseURL
	expensive := false
	if avg, ok := averageTTFT(baseURL); ok {
		expensive = avg > preheatCompactionSlowTTFTMs
	}
	if !expensive && !isLocalModelBaseURL(baseURL) {
		return false
	}
	// c) The pending incremental source is large enough to be worth folding.
	source, _, _, _ := sc.compactionIncrementalSource(true)
	if EstimateContextTokens(source, nil) < preheatCompactionMinIncrementTokens {
		return false
	}
	return true
}

// isLocalModelBaseURL reports whether baseURL points at a loopback host
// (127.0.0.1 / localhost), i.e. a locally served model.
func isLocalModelBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost"
}

// preheatCompactionIfNeeded triggers a background context fold after a
// successful run so the next request's projection is already ready (T-B2).
// It is best-effort and non-blocking: the fold runs in a goroutine with
// recover, serialized per session, and any error is logged but never fails
// the run.
func preheatCompactionIfNeeded(ctx context.Context, conversation Conversation, cfg *config.Config, agentKind string) {
	if !shouldPreheatCompaction(ctx, cfg, agentKind, conversation) {
		return
	}
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil || sc.session == nil {
		return
	}
	sessionID := sc.session.ID
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Warn("preheat_compaction_panic_recovered", slog.String("agent_kind", agentKind), slog.String("session_id", sessionID), slog.Any("error", recovered))
			}
		}()
		unlock := preheatSessionLocks.Lock(sessionID)
		defer unlock()
		folder, ok := conversation.(FoldConversation)
		if !ok || folder == nil {
			return
		}
		slog.Info("preheat_compaction_triggered", slog.String("agent_kind", agentKind), slog.String("session_id", sessionID))
		_, result, err := folder.FoldContextIfNeeded(ctx, ContextCompactionInput{
			Messages: sc.session.GetEffectiveMessages(),
			Force:    false,
		})
		if err != nil {
			slog.Warn("preheat_compaction_failed", slog.String("agent_kind", agentKind), slog.String("session_id", sessionID), slog.Any("error", err))
			return
		}
		slog.Info("preheat_compaction_completed", slog.String("agent_kind", agentKind), slog.String("session_id", sessionID), slog.String("skipped_reason", result.SkippedReason), slog.Int("tokens_before", result.TokensBefore), slog.Int("tokens_after", result.TokensAfter))
	}()
}
