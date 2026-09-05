package config

import "strings"

const (
	// DefaultContextCompactionRetainedTurns is the raw-history tail kept next to
	// a compaction summary when the user has not configured a value.
	DefaultContextCompactionRetainedTurns = 1
	MaxContextCompactionRetainedTurns     = 30

	AgentContextCompactionStrategySummaryAgent = "summary_agent"

	// DefaultMemoryNotesEnabled is the default state of the background memory-note
	// writer (Plan.md M7). When enabled, a background sub-agent writes structured
	// notes while a conversation progresses and compaction reuses them to seed the
	// next checkpoint instead of re-summarizing raw turns.
	DefaultMemoryNotesEnabled = true

	// 借鉴 ACP / billion-context 三层分层压缩（Plan.md §12）的默认触发阈值（token 数）。
	// T1 capture：原始消息池超过该值时触发第一层捕获式压缩（~45× 目标）。
	// T2 distill：T1 摘要池超过该值时触发第二层蒸馏（~10×）。
	// T3 condense：T2 摘要池超过该值时触发第三层浓缩（~5×）。
	DefaultTieredT1ThresholdTokens = float64(24_000)
	DefaultTieredT2ThresholdTokens = float64(8_000)
	DefaultTieredT3ThresholdTokens = float64(3_000)
)

// AgentContextSettings stores per-agent context compaction settings.
type AgentContextSettings struct {
	Default             AgentContextOverride `toml:"default,omitempty" json:"default,omitempty"`
	IDE                 AgentContextOverride `toml:"ide,omitempty" json:"ide,omitempty"`
	InteractiveStory    AgentContextOverride `toml:"interactive_story,omitempty" json:"interactive_story,omitempty"`
	ConfigManager       AgentContextOverride `toml:"config_manager,omitempty" json:"config_manager,omitempty"`
	InteractiveDirector AgentContextOverride `toml:"interactive_director,omitempty" json:"interactive_director,omitempty"`
	VersionSummary      AgentContextOverride `toml:"version_summary,omitempty" json:"version_summary,omitempty"`
	ToolAgent           AgentContextOverride `toml:"tool_agent,omitempty" json:"tool_agent,omitempty"`
	Image               AgentContextOverride `toml:"image,omitempty" json:"image,omitempty"`
	Automation          AgentContextOverride `toml:"automation,omitempty" json:"automation,omitempty"`
	ContextCompaction   AgentContextOverride `toml:"context_compaction,omitempty" json:"context_compaction,omitempty"`
	ImageAnalysis       AgentContextOverride `toml:"image_analysis,omitempty" json:"image_analysis,omitempty"`
}

type AgentContextOverride struct {
	CompactionEnabled          *bool    `toml:"compaction_enabled,omitempty" json:"compaction_enabled,omitempty"`
	CompactionStrategy         *string  `toml:"compaction_strategy,omitempty" json:"compaction_strategy,omitempty"`
	CompactionThreshold        *float64 `toml:"compaction_threshold,omitempty" json:"compaction_threshold,omitempty"`
	CompactionRecentTurns      *int     `toml:"compaction_recent_turns,omitempty" json:"compaction_recent_turns,omitempty"`
	CompactionTargetMin        *float64 `toml:"compaction_target_min_ratio,omitempty" json:"compaction_target_min_ratio,omitempty"`
	CompactionTargetMax        *float64 `toml:"compaction_target_max_ratio,omitempty" json:"compaction_target_max_ratio,omitempty"`
	ToolResultRetentionEnabled *bool    `toml:"tool_result_retention_enabled,omitempty" json:"tool_result_retention_enabled,omitempty"`
	MemoryNotesEnabled         *bool    `toml:"memory_notes_enabled,omitempty" json:"memory_notes_enabled,omitempty"`
	// 借鉴 ACP / billion-context（Plan.md §12）：
	// ContextToolsEnabled 控制模型自主压缩工具集（compress/search_context/acp_status）是否注入，默认开启。
	ContextToolsEnabled *bool `toml:"context_tools_enabled,omitempty" json:"context_tools_enabled,omitempty"`
	// TieredCompactionEnabled 控制三层分层压缩（T1 capture/T2 distill/T3 condense）是否启用，默认关闭（叠加在单层摘要之上）。
	TieredCompactionEnabled *bool    `toml:"tiered_compaction_enabled,omitempty" json:"tiered_compaction_enabled,omitempty"`
	TieredT1Threshold       *float64 `toml:"tiered_t1_threshold_tokens,omitempty" json:"tiered_t1_threshold_tokens,omitempty"`
	TieredT2Threshold       *float64 `toml:"tiered_t2_threshold_tokens,omitempty" json:"tiered_t2_threshold_tokens,omitempty"`
	TieredT3Threshold       *float64 `toml:"tiered_t3_threshold_tokens,omitempty" json:"tiered_t3_threshold_tokens,omitempty"`
	// CompactionQualityGateEnabled 控制压缩质量门（rouge-recall 风格 L1+L2，告警非阻断），默认关闭。
	CompactionQualityGateEnabled *bool `toml:"compaction_quality_gate_enabled,omitempty" json:"compaction_quality_gate_enabled,omitempty"`
	// PreheatCompactionEnabled 控制 run 后后台预热压缩（auto=按运行时信号推导，on=总是预热，off=从不预热），默认 auto。
	PreheatCompactionEnabled *string `toml:"preheat_compaction_enabled,omitempty" json:"preheat_compaction_enabled,omitempty"`
}

type ResolvedAgentContextSettings struct {
	CompactionEnabled            bool    `json:"compaction_enabled"`
	CompactionStrategy           string  `json:"compaction_strategy"`
	CompactionThreshold          float64 `json:"compaction_threshold"`
	CompactionRecentTurns        int     `json:"compaction_recent_turns"`
	CompactionTargetMin          float64 `json:"compaction_target_min_ratio"`
	CompactionTargetMax          float64 `json:"compaction_target_max_ratio"`
	ToolResultRetentionEnabled   bool    `json:"tool_result_retention_enabled"`
	MemoryNotesEnabled           bool    `json:"memory_notes_enabled"`
	ContextToolsEnabled          bool    `json:"context_tools_enabled"`
	TieredCompactionEnabled      bool    `json:"tiered_compaction_enabled"`
	TieredT1Threshold            float64 `json:"tiered_t1_threshold_tokens"`
	TieredT2Threshold            float64 `json:"tiered_t2_threshold_tokens"`
	TieredT3Threshold            float64 `json:"tiered_t3_threshold_tokens"`
	CompactionQualityGateEnabled bool    `json:"compaction_quality_gate_enabled"`
	PreheatCompactionEnabled     string  `json:"preheat_compaction_enabled"`
}

func DefaultAgentContextSettings() AgentContextSettings {
	return AgentContextSettings{
		Default: AgentContextOverride{
			CompactionEnabled:     boolPtr(true),
			CompactionStrategy:    stringPtr(AgentContextCompactionStrategySummaryAgent),
			CompactionThreshold:   floatPtr(0.90),
			CompactionRecentTurns: intPtr(DefaultContextCompactionRetainedTurns),
			CompactionTargetMin:   floatPtr(0.05),
			CompactionTargetMax:   floatPtr(0.20),
		},
	}
}

func MergeAgentContextSettings(parent, child AgentContextSettings) AgentContextSettings {
	return AgentContextSettings{
		Default:             mergeAgentContextOverride(parent.Default, child.Default),
		IDE:                 mergeAgentContextOverride(parent.IDE, child.IDE),
		InteractiveStory:    mergeAgentContextOverride(parent.InteractiveStory, child.InteractiveStory),
		ConfigManager:       mergeAgentContextOverride(parent.ConfigManager, child.ConfigManager),
		InteractiveDirector: mergeAgentContextOverride(parent.InteractiveDirector, child.InteractiveDirector),
		VersionSummary:      mergeAgentContextOverride(parent.VersionSummary, child.VersionSummary),
		ToolAgent:           mergeAgentContextOverride(parent.ToolAgent, child.ToolAgent),
		Image:               mergeAgentContextOverride(parent.Image, child.Image),
		Automation:          mergeAgentContextOverride(parent.Automation, child.Automation),
		ContextCompaction:   mergeAgentContextOverride(parent.ContextCompaction, child.ContextCompaction),
		ImageAnalysis:       mergeAgentContextOverride(parent.ImageAnalysis, child.ImageAnalysis),
	}
}

func ResolveAgentContext(cfg *Config, agentKind string) ResolvedAgentContextSettings {
	settings := DefaultAgentContextSettings()
	if cfg != nil {
		settings = MergeAgentContextSettings(settings, cfg.AgentContexts)
	}
	override := mergeAgentContextOverride(settings.Default, agentContextOverrideFor(settings, agentKind))
	compactionEnabled := true
	if override.CompactionEnabled != nil {
		compactionEnabled = *override.CompactionEnabled
	}
	compactionStrategy := AgentContextCompactionStrategySummaryAgent
	if override.CompactionStrategy != nil {
		compactionStrategy = normalizeCompactionStrategy(*override.CompactionStrategy)
	}
	memoryNotesEnabled := DefaultMemoryNotesEnabled
	if override.MemoryNotesEnabled != nil {
		memoryNotesEnabled = *override.MemoryNotesEnabled
	}
	compactionThreshold := 0.90
	if override.CompactionThreshold != nil {
		compactionThreshold = *override.CompactionThreshold
	}
	if compactionThreshold < 0.50 {
		compactionThreshold = 0.50
	}
	if compactionThreshold > 0.98 {
		compactionThreshold = 0.98
	}
	compactionRecentTurns := DefaultContextCompactionRetainedTurns
	if override.CompactionRecentTurns != nil {
		compactionRecentTurns = normalizeCompactionRetainedTurns(*override.CompactionRecentTurns)
	}
	compactionTargetMin := 0.05
	if override.CompactionTargetMin != nil {
		compactionTargetMin = *override.CompactionTargetMin
	}
	compactionTargetMin = clampCompactionTargetRatio(compactionTargetMin, 0.05)
	compactionTargetMax := 0.20
	if override.CompactionTargetMax != nil {
		compactionTargetMax = *override.CompactionTargetMax
	}
	compactionTargetMax = clampCompactionTargetRatio(compactionTargetMax, 0.20)
	if compactionTargetMax < compactionTargetMin {
		compactionTargetMax = compactionTargetMin
	}
	toolResultRetentionEnabled := defaultToolResultRetentionEnabled(agentKind)
	if override.ToolResultRetentionEnabled != nil {
		toolResultRetentionEnabled = *override.ToolResultRetentionEnabled
	}
	// 借鉴 ACP / billion-context（Plan.md §12）：模型自主压缩工具集默认开启，
	// 分层压缩与质量门默认关闭（叠加在既有单层摘要之上，可配置开启）。
	contextToolsEnabled := true
	if override.ContextToolsEnabled != nil {
		contextToolsEnabled = *override.ContextToolsEnabled
	}
	tieredCompactionEnabled := false
	if override.TieredCompactionEnabled != nil {
		tieredCompactionEnabled = *override.TieredCompactionEnabled
	}
	tieredT1Threshold := DefaultTieredT1ThresholdTokens
	if override.TieredT1Threshold != nil {
		tieredT1Threshold = *override.TieredT1Threshold
	}
	tieredT2Threshold := DefaultTieredT2ThresholdTokens
	if override.TieredT2Threshold != nil {
		tieredT2Threshold = *override.TieredT2Threshold
	}
	tieredT3Threshold := DefaultTieredT3ThresholdTokens
	if override.TieredT3Threshold != nil {
		tieredT3Threshold = *override.TieredT3Threshold
	}
	compactionQualityGateEnabled := false
	if override.CompactionQualityGateEnabled != nil {
		compactionQualityGateEnabled = *override.CompactionQualityGateEnabled
	}
	// 预热压缩默认 auto（按运行时信号推导）；override 仅接受 auto/on/off，非法值回退 auto。
	preheatCompactionEnabled := "auto"
	if override.PreheatCompactionEnabled != nil {
		switch v := strings.TrimSpace(*override.PreheatCompactionEnabled); v {
		case "auto", "on", "off":
			preheatCompactionEnabled = v
		}
	}
	return ResolvedAgentContextSettings{
		CompactionEnabled:            compactionEnabled,
		CompactionStrategy:           compactionStrategy,
		CompactionThreshold:          compactionThreshold,
		CompactionRecentTurns:        compactionRecentTurns,
		CompactionTargetMin:          compactionTargetMin,
		CompactionTargetMax:          compactionTargetMax,
		ToolResultRetentionEnabled:   toolResultRetentionEnabled,
		MemoryNotesEnabled:           memoryNotesEnabled,
		ContextToolsEnabled:          contextToolsEnabled,
		TieredCompactionEnabled:      tieredCompactionEnabled,
		TieredT1Threshold:            tieredT1Threshold,
		TieredT2Threshold:            tieredT2Threshold,
		TieredT3Threshold:            tieredT3Threshold,
		CompactionQualityGateEnabled: compactionQualityGateEnabled,
		PreheatCompactionEnabled:     preheatCompactionEnabled,
	}
}

func mergeAgentContextOverride(parent, child AgentContextOverride) AgentContextOverride {
	out := parent
	if child.CompactionEnabled != nil {
		out.CompactionEnabled = child.CompactionEnabled
	}
	if child.CompactionStrategy != nil {
		out.CompactionStrategy = child.CompactionStrategy
	}
	if child.CompactionThreshold != nil {
		out.CompactionThreshold = child.CompactionThreshold
	}
	if child.CompactionRecentTurns != nil {
		out.CompactionRecentTurns = child.CompactionRecentTurns
	}
	if child.CompactionTargetMin != nil {
		out.CompactionTargetMin = child.CompactionTargetMin
	}
	if child.CompactionTargetMax != nil {
		out.CompactionTargetMax = child.CompactionTargetMax
	}
	if child.ToolResultRetentionEnabled != nil {
		out.ToolResultRetentionEnabled = child.ToolResultRetentionEnabled
	}
	if child.MemoryNotesEnabled != nil {
		out.MemoryNotesEnabled = child.MemoryNotesEnabled
	}
	if child.ContextToolsEnabled != nil {
		out.ContextToolsEnabled = child.ContextToolsEnabled
	}
	if child.TieredCompactionEnabled != nil {
		out.TieredCompactionEnabled = child.TieredCompactionEnabled
	}
	if child.TieredT1Threshold != nil {
		out.TieredT1Threshold = child.TieredT1Threshold
	}
	if child.TieredT2Threshold != nil {
		out.TieredT2Threshold = child.TieredT2Threshold
	}
	if child.TieredT3Threshold != nil {
		out.TieredT3Threshold = child.TieredT3Threshold
	}
	if child.CompactionQualityGateEnabled != nil {
		out.CompactionQualityGateEnabled = child.CompactionQualityGateEnabled
	}
	if child.PreheatCompactionEnabled != nil {
		out.PreheatCompactionEnabled = child.PreheatCompactionEnabled
	}
	return out
}

func agentContextOverrideFor(settings AgentContextSettings, agentKind string) AgentContextOverride {
	if definition, ok := LookupAgentKind(agentKind); ok && definition.ContextOverride != nil {
		return definition.ContextOverride(settings)
	}
	return AgentContextOverride{}
}

func sanitizeAgentContextSettings(settings AgentContextSettings) AgentContextSettings {
	settings.Default = sanitizeAgentContextOverride(settings.Default)
	settings.IDE = sanitizeAgentContextOverride(settings.IDE)
	settings.InteractiveStory = sanitizeAgentContextOverride(settings.InteractiveStory)
	settings.ConfigManager = sanitizeAgentContextOverride(settings.ConfigManager)
	settings.InteractiveDirector = sanitizeAgentContextOverride(settings.InteractiveDirector)
	settings.VersionSummary = sanitizeAgentContextOverride(settings.VersionSummary)
	settings.ToolAgent = sanitizeAgentContextOverride(settings.ToolAgent)
	settings.Image = sanitizeAgentContextOverride(settings.Image)
	settings.Automation = sanitizeAgentContextOverride(settings.Automation)
	settings.ContextCompaction = sanitizeAgentContextOverride(settings.ContextCompaction)
	return settings
}

func sanitizeAgentContextOverride(override AgentContextOverride) AgentContextOverride {
	if override.CompactionThreshold != nil {
		if *override.CompactionThreshold < 0.50 {
			*override.CompactionThreshold = 0.50
		}
		if *override.CompactionThreshold > 0.98 {
			*override.CompactionThreshold = 0.98
		}
	}
	if override.CompactionStrategy != nil {
		*override.CompactionStrategy = normalizeCompactionStrategy(*override.CompactionStrategy)
	}
	if override.CompactionRecentTurns != nil {
		*override.CompactionRecentTurns = normalizeCompactionRetainedTurns(*override.CompactionRecentTurns)
	}
	if override.CompactionTargetMin != nil {
		*override.CompactionTargetMin = clampCompactionTargetRatio(*override.CompactionTargetMin, 0.05)
	}
	if override.CompactionTargetMax != nil {
		*override.CompactionTargetMax = clampCompactionTargetRatio(*override.CompactionTargetMax, 0.20)
	}
	if override.CompactionTargetMin != nil && override.CompactionTargetMax != nil && *override.CompactionTargetMax < *override.CompactionTargetMin {
		*override.CompactionTargetMax = *override.CompactionTargetMin
	}
	return override
}

func normalizeCompactionStrategy(value string) string {
	switch value {
	case AgentContextCompactionStrategySummaryAgent:
		return value
	default:
		return AgentContextCompactionStrategySummaryAgent
	}
}

func normalizeCompactionRetainedTurns(value int) int {
	if value <= 0 {
		return DefaultContextCompactionRetainedTurns
	}
	if value > MaxContextCompactionRetainedTurns {
		return MaxContextCompactionRetainedTurns
	}
	return value
}

func clampCompactionTargetRatio(value, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	if value < 0.01 {
		return 0.01
	}
	if value > 0.80 {
		return 0.80
	}
	return value
}

func defaultToolResultRetentionEnabled(agentKind string) bool {
	switch agentKind {
	case AgentKindIDE, AgentKindInteractiveStory:
		return true
	default:
		return false
	}
}
