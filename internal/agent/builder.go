package agent

import (
	"context"
	"fmt"
	"log"
	"strings"

	localbk "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	filesystemmw "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"

	"denova/config"
	agenttools "denova/internal/agent/tools"
	"denova/internal/book"
	"denova/internal/prompts"
	"denova/internal/providercompat"
	novaskills "denova/internal/skills"
	"denova/internal/workspacechange"
)

var newDeepAgent = deep.New

const unlimitedAgentMaxIterations = 1_000_000

// Build 构建小说创作 Agent（deep agent + 文件系统工具 + Skill 中间件）。
func Build(ctx context.Context, cfg *config.Config, state *book.State, teller IDEStoryTeller) (adk.Agent, error) {
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindIDE,
		Name:              "DenovaAgent",
		Description:       "AI 小说创作助手",
		Instruction:       BuildInstruction(cfg, state, teller),
		EnableSkills:      true,
		ExtraToolsFactory: ideToolsFactory(cfg),
	})
}

func BuildInteractiveStory(ctx context.Context, cfg *config.Config, state *book.State, teller prompts.InteractiveStorySystemInstructionInput, toolContexts ...InteractiveStoryToolContext) (adk.Agent, error) {
	handlers := []adk.ChatModelAgentMiddleware{newInteractiveStoryToolMiddleware()}
	var outputGuard func(context.Context, *adk.RetryContext) *adk.RetryDecision
	if len(toolContexts) > 0 && toolContexts[0].TurnResultReady != nil {
		completionTokens, _ := EstimateContextProjectionReserves(cfg, config.AgentKindInteractiveStory, teller.ReplyTargetChars)
		handlers = append(handlers, newInteractiveTurnProtocolMiddleware(toolContexts[0].TurnResultReady, completionTokens))
		outputGuard = newInteractiveCompletionGuard(toolContexts[0].TurnResultReady)
	}
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindInteractiveStory,
		Name:              "DenovaInteractiveStoryAgent",
		Description:       "AI 互动故事叙事助手",
		Instruction:       BuildInteractiveStoryInstruction(cfg, state, teller),
		EnableSkills:      true,
		DisableWriteTodos: true,
		ExtraHandlers:     handlers,
		ExtraToolsFactory: interactiveStoryToolsFactory(cfg, toolContexts...),
		ModelOutputGuard:  outputGuard,
	})
}

func BuildInteractiveDirector(ctx context.Context, cfg *config.Config, state *book.State, toolContexts ...InteractiveStoryToolContext) (adk.Agent, error) {
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindInteractiveDirector,
		Name:              "DenovaInteractiveDirectorAgent",
		Description:       "AI 互动故事后台导演",
		Instruction:       protectedSystemInstruction(cfg, config.AgentKindInteractiveDirector, prompts.BuildInteractiveDirectorSystemInstruction()),
		EnableSkills:      false,
		DisableWriteTodos: true,
		ExtraHandlers:     []adk.ChatModelAgentMiddleware{newInteractiveDirectorPlanFileMiddleware()},
		ExtraToolsFactory: interactiveDirectorToolsFactory(cfg, toolContexts...),
	})
}

// BuildConfigManagerAgent 构建统一配置管理 Agent（deep agent + 通用工具 + Skill + 模块资源工具）。
func BuildConfigManagerAgent(ctx context.Context, cfg *config.Config, state *book.State, resourceSkills ...ConfigManagerResourceSkill) (adk.Agent, error) {
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindConfigManager,
		Name:              "DenovaConfigManagerAgent",
		Description:       "AI 配置与资源管理助手",
		Instruction:       BuildConfigManagerInstruction(cfg, state, resourceSkills...),
		EnableSkills:      true,
		ExtraToolsFactory: configManagerToolsFactory(cfg),
	})
}

// BuildAutomationAgent 构建后台自动化 Agent。工具权限由调用方按任务写入策略提前收敛到 cfg.AgentTools.Automation。
func BuildAutomationAgent(ctx context.Context, cfg *config.Config, state *book.State, task AutomationTaskInstruction) (adk.Agent, error) {
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindAutomation,
		Name:              "DenovaAutomationAgent",
		Description:       "AI 自动化任务助手",
		Instruction:       BuildAutomationInstruction(cfg, state, task),
		EnableSkills:      true,
		ExtraToolsFactory: loreToolsFactory(cfg, false),
	})
}

// BuildImageAgent 构建通用图像 Agent。调用方通过运行时上下文和 Skill 约束具体用途。
func BuildImageAgent(ctx context.Context, cfg *config.Config, state *book.State, systemPrompt string) (adk.Agent, error) {
	return buildDeepAgent(ctx, cfg, deepAgentSpec{
		Kind:              config.AgentKindImage,
		Name:              "DenovaImageAgent",
		Description:       "AI 图像生成助手",
		Instruction:       BuildImageInstruction(cfg, state, systemPrompt),
		EnableSkills:      true,
		DisableWriteTodos: true,
		ExtraToolsFactory: imageToolsFactory(cfg),
	})
}

type deepAgentSpec struct {
	Kind              string
	Name              string
	Description       string
	Instruction       string
	EnableSkills      bool
	DisableWriteTodos bool
	ExtraHandlers     []adk.ChatModelAgentMiddleware
	ExtraTools        []tool.BaseTool
	ExtraToolsFactory func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error)
	ModelOutputGuard  func(context.Context, *adk.RetryContext) *adk.RetryDecision
}

func buildDeepAgent(ctx context.Context, cfg *config.Config, spec deepAgentSpec) (adk.Agent, error) {
	resolved := resolvedModelSettingsForAgent(cfg, spec.Kind)
	modelCfg := chatModelConfigFromResolved(resolved)
	toolSettings := config.ResolveAgentTools(cfg, spec.Kind)
	cm, err := openai.NewChatModel(ctx, &modelCfg)
	if err != nil {
		return nil, fmt.Errorf("创建模型失败: %w", err)
	}
	// providercompat 决定是否要为这个 provider 加包装层（修复工具调用格式、剥离内联 think 等）。
	// agent 包不感知具体 provider；新增 provider 的兼容性处理只需在 providercompat 里加。
	chatModel := providercompat.Wrap(cm, modelCfg)

	// When the model profile has disable_tools=true, skip all tool assembly.
	// This prevents the eino framework from injecting tool definitions into
	// model requests, which avoids triggering llama.cpp's native PEG tool-call
	// grammar on local models that don't support structured tool calling.
	disableTools := resolved.DisableTools

	var assembly chatModelAgentAssembly
	var subAgents []adk.Agent
	if !disableTools {
		assembly, err = buildChatModelAgentAssembly(ctx, cfg, chatModelAgentAssemblySpec{
			Kind:              spec.Kind,
			ModelCfg:          modelCfg,
			ToolSettings:      toolSettings,
			EnableSkills:      spec.EnableSkills,
			ExtraHandlers:     spec.ExtraHandlers,
			ExtraTools:        spec.ExtraTools,
			ExtraToolsFactory: spec.ExtraToolsFactory,
			IncludeCompaction: true,
		})
		if err != nil {
			return nil, err
		}
		subAgents, err = buildConfiguredSubAgents(ctx, cfg, spec, toolSettings)
		if err != nil {
			return nil, err
		}
	} else {
		log.Printf("[agent] disable_tools=true for agent=%s model=%s, skipping tool assembly", spec.Kind, modelCfg.Model)
	}

	toolsConfig := adk.ToolsConfig{}
	if !disableTools {
		toolsConfig = adk.ToolsConfig{
			EmitInternalEvents: true,
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               assembly.Tools,
				UnknownToolsHandler: handleUnknownTool,
			},
		}
	}

	instruction := spec.Instruction
	// 复合模型：interactive_story 配置了 writer_profile_id 时，自动挂载一个
	// tool-less 的叙事写手子 Agent，并把纯叙事文本生成委派给它。写手模型强制
	// disable_tools，请求 body 不含 tools，避免本地模型的 PEG tool-call grammar
	// 与自由文本（如 markdown 代码块）输出冲突。主模型仍负责协议工具与状态管理。
	if spec.Kind == config.AgentKindInteractiveStory && resolved.WriterProfileID != "" && !disableTools {
		writerAgent, writerErr := buildWriterSubAgent(ctx, cfg, resolved.WriterProfileID)
		if writerErr != nil {
			return nil, writerErr
		}
		subAgents = append(subAgents, writerAgent)
		instruction = instruction + buildWriterDelegationInstruction()
		log.Printf("[agent] interactive_story writer sub-agent enabled writer_profile=%s", resolved.WriterProfileID)
	}

	return newDeepAgent(ctx, &deep.Config{
		Name:                   spec.Name,
		Description:            spec.Description,
		ChatModel:              chatModel,
		Instruction:            instruction,
		SubAgents:              subAgents,
		WithoutWriteTodos:      disableTools || spec.DisableWriteTodos || !toolSettings.Todo,
		WithoutGeneralSubAgent: disableTools || !config.GeneralSubAgentEnabled(cfg, spec.Kind),
		MaxIteration:           configMaxIteration(cfg),
		Handlers:               assembly.Handlers,
		ToolsConfig:            toolsConfig,
		ModelRetryConfig:       modelRetryConfig(cfg, spec.ModelOutputGuard),
	})
}

// writerSubAgentID 是叙事写手子 Agent 的稳定 ID，主模型通过 transfer 委派给它。
const writerSubAgentID = "narrative-writer"

// buildWriterDelegationInstruction 构建追加到主 Agent instruction 的委派指引，
// 指引其将纯叙事文本生成委派给写手子 Agent，而协议工具调用、状态管理仍由主模型完成。
func buildWriterDelegationInstruction() string {
	return fmt.Sprintf(`

## 叙事写手委派（复合模型）

你配置了一个名为 %q 的叙事写手子 Agent，它使用独立的本地模型，专门负责纯叙事文本生成。

- 当需要生成**叙事正文、场景描写、人物对话、心理活动**等纯文学文本时，请通过 transfer 把该写作任务委派给 %q 子 Agent，由它产出文本后你再包装提交。
- **协议工具调用**（如 submit_interactive_turn）、**资料库读写**、**状态更新**、**上下文压缩**等仍由你（主模型）亲自处理，不要委派给写手。
- 委派时，把必要的上下文（当前场景、角色状态、文风要求、字数目标、前文衔接要点）一并传给写手，使其无需访问工具即可独立写作。
- 写手返回的文本是初稿素材，最终是否符合协议格式、是否需要调整，由你判断并负责提交。
`, writerSubAgentID, writerSubAgentID)
}

type chatModelAgentAssemblySpec struct {
	Kind              string
	ToolPolicyKind    string
	ModelCfg          openai.ChatModelConfig
	ToolSettings      config.ResolvedAgentToolSettings
	EnableSkills      bool
	ExtraHandlers     []adk.ChatModelAgentMiddleware
	ExtraTools        []tool.BaseTool
	ExtraToolsFactory func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error)
	IncludeCompaction bool
}

type chatModelAgentAssembly struct {
	Tools    []tool.BaseTool
	Handlers []adk.ChatModelAgentMiddleware
}

func buildChatModelAgentAssembly(ctx context.Context, cfg *config.Config, spec chatModelAgentAssemblySpec) (chatModelAgentAssembly, error) {
	localBackend, err := localbk.NewBackend(ctx, &localbk.Config{})
	if err != nil {
		return chatModelAgentAssembly{}, fmt.Errorf("创建 backend 失败: %w", err)
	}
	workspace := ""
	if cfg != nil {
		workspace = cfg.Workspace
	}
	backend := newAgentFilesystemBackend(localBackend, workspace)
	executionGate := sharedToolExecutionGate(workspace)
	settings := spec.ToolSettings
	middlewares := []agenttools.MiddlewareRegistration{
		{
			Name:    "filesystem",
			Enabled: agenttools.FilesystemAllowed,
			Build: func(ctx context.Context, _ agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				return newFilesystemMiddleware(ctx, backend, newAgentStreamingShell(workspace), spec.ToolSettings, workspace)
			},
		},
		{
			Name:    "skills",
			Enabled: agenttools.CapabilityAllowed(config.AgentToolSkills),
			Build: func(ctx context.Context, settings agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				return newSkillMiddleware(ctx, cfg, spec.Kind, spec.EnableSkills, settings)
			},
		},
	}
	middlewares = append(middlewares, staticMiddlewareRegistrations("extra_handler", spec.ExtraHandlers)...)
	middlewares = append(middlewares,
		agenttools.MiddlewareRegistration{
			Name: "context_compaction",
			Enabled: func(agenttools.Settings) bool {
				return spec.IncludeCompaction
			},
			Build: func(context.Context, agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				return &contextCompactionMiddleware{
					BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
					agentKind:                    spec.Kind,
				}, nil
			},
		},
		agenttools.MiddlewareRegistration{
			Name: "tool_orchestrator",
			Build: func(context.Context, agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				resultStore, err := newResultStoreForWorkspace(workspace)
				if err != nil {
					return nil, err
				}
				return &toolOrchestratorMiddleware{
					agentKind:                 spec.Kind,
					policyKind:                firstNonEmpty(spec.ToolPolicyKind, spec.Kind),
					workspace:                 workspace,
					toolSettings:              spec.ToolSettings,
					enforceToolSettings:       true,
					toolResultMaxBytes:        configToolResultMaxBytes(cfg),
					toolResultBatchLimitBytes: configToolResultBatchLimitBytes(cfg),
					executionGate:             executionGate,
					resultStore:               resultStore,
				}, nil
			},
		},
		agenttools.MiddlewareRegistration{
			Name: "model_input_logging",
			Build: func(context.Context, agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				return &modelInputLoggingMiddleware{
					BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
					agentKind:                    spec.Kind,
					config:                       spec.ModelCfg,
				}, nil
			},
		},
	)
	toolRegistrations := []agenttools.ToolRegistration{
		agenttools.StaticTools("extra_tools", spec.ExtraTools...),
	}
	if spec.ExtraToolsFactory != nil {
		toolRegistrations = append(toolRegistrations, agenttools.ToolRegistration{
			Name:  "extra_tools_factory",
			Build: spec.ExtraToolsFactory,
		})
	}
	toolRegistrations = append(toolRegistrations, agenttools.ToolRegistration{
		Name:    "web_search",
		Enabled: stableWebSearchSchemaAllowed(firstNonEmpty(spec.ToolPolicyKind, spec.Kind)),
		Build: func(agenttools.Settings) ([]tool.BaseTool, error) {
			return newWebSearchTools()
		},
	})
	toolRegistrations = append(toolRegistrations, agenttools.ToolRegistration{
		Name:    "count_words",
		Enabled: agenttools.CapabilityAllowed(config.AgentToolFileRead),
		Build: func(agenttools.Settings) ([]tool.BaseTool, error) {
			countTool, err := newCountWordsTool(workspace)
			if err != nil {
				return nil, fmt.Errorf("创建 count_words 工具失败: %w", err)
			}
			return []tool.BaseTool{countTool}, nil
		},
	})
	toolRegistrations = append(toolRegistrations, agenttools.ToolRegistration{
		Name:    "list_aliases",
		Enabled: agenttools.CapabilityAllowed(config.AgentToolFileRead),
		Build: func(agenttools.Settings) ([]tool.BaseTool, error) {
			aliasTool, err := newListAliasesTool(workspace)
			if err != nil {
				return nil, fmt.Errorf("创建 list_aliases 工具失败: %w", err)
			}
			return []tool.BaseTool{aliasTool}, nil
		},
	})
	// 模型自主压缩工具（Plan.md §12 / T1）：compress / search_context / acp_status。
	// 由 [agent] context_tools_enabled 开关控制，默认开启；与系统阈值压缩叠加而非互斥。
	toolRegistrations = append(toolRegistrations, agenttools.ToolRegistration{
		Name: "context_tools",
		Enabled: func(agenttools.Settings) bool {
			return config.ResolveAgentContext(cfg, spec.Kind).ContextToolsEnabled
		},
		Build: func(agenttools.Settings) ([]tool.BaseTool, error) {
			return NewContextTools(cfg, spec.Kind), nil
		},
	})
	assembly, err := agenttools.Build(ctx, agenttools.BuildRequest{
		Settings:    settings,
		Middlewares: middlewares,
		Tools:       toolRegistrations,
	})
	if err != nil {
		return chatModelAgentAssembly{}, err
	}
	return chatModelAgentAssembly{Tools: assembly.Tools, Handlers: assembly.Handlers}, nil
}

func staticMiddlewareRegistrations(prefix string, handlers []adk.ChatModelAgentMiddleware) []agenttools.MiddlewareRegistration {
	registrations := make([]agenttools.MiddlewareRegistration, 0, len(handlers))
	for i, handler := range handlers {
		handler := handler
		name := fmt.Sprintf("%s_%d", prefix, i+1)
		registrations = append(registrations, agenttools.MiddlewareRegistration{
			Name: name,
			Build: func(context.Context, agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
				return handler, nil
			},
		})
	}
	return registrations
}

func newSkillMiddleware(ctx context.Context, cfg *config.Config, agentKind string, enabled bool, settings agenttools.Settings) (adk.ChatModelAgentMiddleware, error) {
	if !enabled || !settings.Skills || cfg == nil {
		return nil, nil
	}
	skillBackend := novaskills.NewAgentBackend(
		novaskills.NewDirectories(cfg.SkillsDir, cfg.DataDir(), cfg.Workspace),
		agentKind,
		config.ResolveAgentSkillOverrides(cfg, agentKind),
	)
	availableSkills, listErr := skillBackend.List(ctx)
	if listErr != nil {
		log.Printf("[agent] 加载 Skills 列表失败 agent=%s err=%v", agentKind, listErr)
		return nil, nil
	}
	if len(availableSkills) == 0 {
		return nil, nil
	}
	skillMw, err := skill.NewMiddleware(ctx, &skill.Config{Backend: skillBackend})
	if err != nil {
		log.Printf("[agent] 创建 Skill middleware 失败 agent=%s err=%v", agentKind, err)
		return nil, nil
	}
	return skillMw, nil
}

func buildConfiguredSubAgents(ctx context.Context, cfg *config.Config, parent deepAgentSpec, parentTools config.ResolvedAgentToolSettings) ([]adk.Agent, error) {
	if cfg == nil || !config.IsDeepAgentParentKind(parent.Kind) {
		return nil, nil
	}
	subConfigs := config.SanitizeSubAgents(cfg.SubAgents)
	if len(subConfigs) == 0 {
		return nil, nil
	}
	subAgents := make([]adk.Agent, 0, len(subConfigs))
	for _, sub := range subConfigs {
		if !config.SubAgentAllowedForParent(sub, parent.Kind) {
			continue
		}
		subAgent, err := buildConfiguredSubAgent(ctx, cfg, parent, parentTools, sub)
		if err != nil {
			return nil, err
		}
		subAgents = append(subAgents, subAgent)
	}
	return subAgents, nil
}

func buildConfiguredSubAgent(ctx context.Context, cfg *config.Config, parent deepAgentSpec, parentTools config.ResolvedAgentToolSettings, sub config.SubAgentConfig) (adk.Agent, error) {
	subResolved := config.ResolveSubAgentModel(cfg, parent.Kind, sub)
	modelCfg := chatModelConfigFromResolved(subResolved)
	cm, err := openai.NewChatModel(ctx, &modelCfg)
	if err != nil {
		return nil, fmt.Errorf("创建子 Agent 模型失败 id=%s: %w", sub.ID, err)
	}
	subChatModel := providercompat.Wrap(cm, modelCfg)
	toolSettings := config.ResolveSubAgentTools(parentTools, sub.Tools)

	var assembly chatModelAgentAssembly
	if !subResolved.DisableTools {
		assembly, err = buildChatModelAgentAssembly(ctx, cfg, chatModelAgentAssemblySpec{
			Kind:              sub.ID,
			ToolPolicyKind:    parent.Kind,
			ModelCfg:          modelCfg,
			ToolSettings:      toolSettings,
			EnableSkills:      parent.EnableSkills,
			ExtraToolsFactory: parent.ExtraToolsFactory,
			IncludeCompaction: false,
		})
		if err != nil {
			return nil, err
		}
	} else {
		log.Printf("[agent] disable_tools=true for sub-agent=%s model=%s, skipping tool assembly", sub.ID, modelCfg.Model)
	}

	toolsConfig := adk.ToolsConfig{}
	if !subResolved.DisableTools {
		toolsConfig = adk.ToolsConfig{
			EmitInternalEvents: true,
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               assembly.Tools,
				UnknownToolsHandler: handleUnknownTool,
			},
		}
	}

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:             sub.ID,
		Description:      sub.Description,
		Instruction:      buildSubAgentInstruction(parent, sub),
		Model:            subChatModel,
		MaxIterations:    configMaxIteration(cfg),
		Handlers:         assembly.Handlers,
		ToolsConfig:      toolsConfig,
		ModelRetryConfig: modelRetryConfig(cfg, nil),
	})
}

// buildWriterSubAgent 构建叙事写手子 Agent。它使用 writerProfileID 指定的模型，
// 强制 disable_tools（请求 body 不含 tools），无 skills、无 sub-agents、无工具，
// 只负责产出纯叙事文本，避免本地模型的 PEG tool-call grammar 与自由文本冲突。
func buildWriterSubAgent(ctx context.Context, cfg *config.Config, writerProfileID string) (adk.Agent, error) {
	writerResolved := config.ResolveProfileModel(cfg, writerProfileID)
	// 写手永远不注入 tools，无论 profile 是否显式配置 disable_tools。
	writerResolved.DisableTools = true
	modelCfg := chatModelConfigFromResolved(writerResolved)
	cm, err := openai.NewChatModel(ctx, &modelCfg)
	if err != nil {
		return nil, fmt.Errorf("创建叙事写手模型失败 profile=%s: %w", writerProfileID, err)
	}
	writerModel := providercompat.Wrap(cm, modelCfg)
	log.Printf("[agent] disable_tools=true for writer sub-agent=%s model=%s, skipping tool assembly", writerSubAgentID, modelCfg.Model)

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        writerSubAgentID,
		Description: "叙事写手：使用本地模型生成纯叙事文本，不访问任何工具。",
		Instruction: writerSubAgentInstruction,
		Model:       writerModel,
		// 写手是单轮纯文本生成，不需要工具迭代循环。
		MaxIterations:    1,
		ToolsConfig:      adk.ToolsConfig{},
		ModelRetryConfig: modelRetryConfig(cfg, nil),
	})
}

// writerSubAgentInstruction 是叙事写手子 Agent 的系统指引，限定其只做纯文本叙事生成。
const writerSubAgentInstruction = `你是互动故事的叙事写手，负责产出高质量的纯叙事文本。

职责：
- 根据主 Agent 提供的上下文（当前场景、角色状态、文风要求、字数目标、前文衔接要点）创作叙事正文。
- 输出场景描写、人物对话、心理活动、动作与环境等文学内容。

约束：
- 你没有任何工具，不要尝试调用工具，也不要输出工具调用格式。
- 直接输出叙事正文本身，不要输出解释、元评论、Markdown 标题或代码块包裹。
- 保持与所提供上下文一致的角色性格、说话方式、情节走向和文风。
- 遵循指定的字数目标，保证叙事完整、自然衔接。`

func modelRetryConfig(cfg *config.Config, outputGuard func(context.Context, *adk.RetryContext) *adk.RetryDecision) *adk.ModelRetryConfig {
	retryConfig := &adk.ModelRetryConfig{
		MaxRetries:  configModelMaxRetries(cfg),
		IsRetryAble: isTransientModelError,
	}
	if outputGuard == nil {
		return retryConfig
	}
	retryConfig.IsRetryAble = nil
	retryConfig.ShouldRetry = func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
		if retryCtx != nil && retryCtx.Err != nil {
			return &adk.RetryDecision{Retry: isTransientModelError(ctx, retryCtx.Err)}
		}
		return outputGuard(ctx, retryCtx)
	}
	return retryConfig
}

func isTransientModelError(_ context.Context, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Rate limiting
	if strings.Contains(msg, "429") ||
		strings.Contains(msg, "Too Many Requests") ||
		strings.Contains(msg, "qpm limit") {
		return true
	}
	// Provider-side tool call JSON parse errors (e.g., llama-server)
	// These are recoverable — the model just needs to regenerate.
	if strings.Contains(msg, "Failed to parse tool call") ||
		strings.Contains(msg, "status code: 500") ||
		strings.Contains(msg, "500 Internal Server Error") {
		return true
	}
	return false
}

func buildSubAgentInstruction(parent deepAgentSpec, sub config.SubAgentConfig) string {
	var sb strings.Builder
	if parentInstruction := strings.TrimSpace(parent.Instruction); parentInstruction != "" {
		sb.WriteString(parentInstruction)
		sb.WriteString("\n\n---\n\n")
	}
	sb.WriteString("# SubAgent 专属说明\n\n")
	sb.WriteString("以下说明只限定当前 SubAgent 的职责、输出形态和工作偏好；不得覆盖父 Agent 的运行时契约、工具权限、workspace 边界、互动禁写规则、输出协议或后端校验。若与父 Agent system prompt 冲突，必须以父 Agent system prompt 为准。\n\n")
	if name := strings.TrimSpace(sub.Name); name != "" {
		sb.WriteString("- 名称：")
		sb.WriteString(name)
		sb.WriteString("\n")
	}
	if id := strings.TrimSpace(sub.ID); id != "" {
		sb.WriteString("- ID：")
		sb.WriteString(id)
		sb.WriteString("\n")
	}
	if description := strings.TrimSpace(sub.Description); description != "" {
		sb.WriteString("- 职责：")
		sb.WriteString(description)
		sb.WriteString("\n")
	}
	if prompt := strings.TrimSpace(sub.SystemPrompt); prompt != "" {
		sb.WriteString("\n## 专属系统提示\n\n")
		sb.WriteString(prompt)
	}
	return strings.TrimSpace(sb.String())
}

func loreToolsFactory(cfg *config.Config, forceReadOnly bool) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(settings config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		if cfg == nil || (!settings.LoreRead && !settings.LoreWrite) {
			return nil, nil
		}
		allowWrite := !forceReadOnly && settings.LoreWrite
		return newLoreTools(cfg.Workspace, allowWrite)
	}
}

func ideToolsFactory(cfg *config.Config) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(_ config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		if cfg == nil {
			return nil, nil
		}
		loreTools, err := newLoreTools(cfg.Workspace, true)
		if err != nil {
			return nil, err
		}
		imageTools, err := newIllustrationTools(cfg)
		if err != nil {
			return nil, err
		}
		imageAnalysisTools, err := newImageAnalysisTools(cfg)
		if err != nil {
			return nil, err
		}
		tools := append([]tool.BaseTool{}, loreTools...)
		tools = append(tools, imageTools...)
		tools = append(tools, imageAnalysisTools...)
		return tools, nil
	}
}

func imageToolsFactory(cfg *config.Config) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(_ config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		if cfg == nil {
			return nil, nil
		}
		return newIllustrationTools(cfg)
	}
}

func interactiveStoryToolsFactory(cfg *config.Config, toolContexts ...InteractiveStoryToolContext) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(_ config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		var tools []tool.BaseTool
		if cfg != nil {
			loreTools, err := newLoreTools(cfg.Workspace, false)
			if err != nil {
				return nil, err
			}
			tools = append(tools, loreTools...)
		}
		if len(toolContexts) > 0 {
			historyTools, err := newInteractiveHistoryTools(toolContexts[0])
			if err != nil {
				return nil, err
			}
			tools = append(tools, historyTools...)
			stateSchemaTools, err := newInteractiveOpeningStateSchemaTools(toolContexts[0])
			if err != nil {
				return nil, err
			}
			tools = append(tools, stateSchemaTools...)
			turnTools, err := newInteractiveTurnTools(toolContexts[0])
			if err != nil {
				return nil, err
			}
			tools = append(tools, turnTools...)
		}
		return tools, nil
	}
}

func interactiveDirectorToolsFactory(cfg *config.Config, toolContexts ...InteractiveStoryToolContext) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(settings config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		var tools []tool.BaseTool
		var storyToolContext InteractiveStoryToolContext
		if len(toolContexts) > 0 {
			storyToolContext = toolContexts[0]
		}
		if cfg != nil && settings.LoreRead {
			var options []loreToolsOptions
			switch strings.TrimSpace(storyToolContext.MaintenanceTask) {
			case "director_plan_update", "opening_plan":
				policy := defaultLoreReadPolicy()
				policy.OnRead = storyToolContext.OnLoreItemsRead
				options = append(options, loreToolsOptions{ReadPolicy: policy})
			}
			loreTools, err := newLoreTools(cfg.Workspace, false, options...)
			if err != nil {
				return nil, err
			}
			tools = append(tools, loreTools...)
		}
		if len(toolContexts) == 0 {
			return tools, nil
		}
		ctx := storyToolContext
		switch strings.TrimSpace(ctx.MaintenanceTask) {
		case "director_plan_update", "opening_plan":
			historyTools, err := newInteractiveHistoryTools(ctx)
			if err != nil {
				return nil, err
			}
			eventTools, err := newInteractiveEventTools(ctx)
			if err != nil {
				return nil, err
			}
			planTools, err := newInteractiveDirectorPlanTools(ctx)
			tools = append(tools, historyTools...)
			tools = append(tools, eventTools...)
			return append(tools, planTools...), err
		default:
			return tools, nil
		}
	}
}

func configManagerToolsFactory(cfg *config.Config) func(config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
	return func(settings config.ResolvedAgentToolSettings) ([]tool.BaseTool, error) {
		if cfg == nil {
			return nil, nil
		}
		if !configManagerFactoryAllowed(settings) {
			return nil, nil
		}
		configTools, err := newConfigManagerTools(cfg, settings)
		if err != nil {
			return nil, err
		}
		return configTools, nil
	}
}

func configManagerFactoryAllowed(settings config.ResolvedAgentToolSettings) bool {
	return settings.LoreRead ||
		settings.LoreWrite ||
		settings.Todo ||
		settings.Skills ||
		settings.AgentConfigRead ||
		settings.AgentConfigWrite
}

func newFilesystemMiddleware(ctx context.Context, backend filesystem.Backend, streamingShell filesystem.StreamingShell, settings config.ResolvedAgentToolSettings, workspaces ...string) (adk.ChatModelAgentMiddleware, error) {
	if backend == nil {
		return nil, nil
	}
	if !settings.FileRead && !settings.FileWrite && !settings.ShellExecute {
		return nil, nil
	}
	workspace := ""
	if len(workspaces) > 0 {
		workspace = strings.TrimSpace(workspaces[0])
	}
	readTool, err := newWorkspaceReadFileTool(backend, workspace)
	if err != nil {
		return nil, fmt.Errorf("创建 read_file 工具失败: %w", err)
	}
	readToolConfig := &filesystemmw.ToolConfig{CustomTool: readTool}
	writeToolConfig := &filesystemmw.ToolConfig{}
	editToolConfig := &filesystemmw.ToolConfig{}
	if workspace != "" && settings.FileWrite {
		changes, err := workspacechange.ForWorkspace(workspace)
		if err != nil {
			return nil, fmt.Errorf("创建 workspace change service 失败: %w", err)
		}
		writeTool, err := newWorkspaceWriteFileTool(changes)
		if err != nil {
			return nil, fmt.Errorf("创建 write_file 工具失败: %w", err)
		}
		editTool, err := newWorkspaceEditFileTool(changes)
		if err != nil {
			return nil, fmt.Errorf("创建 edit_file 工具失败: %w", err)
		}
		writeToolConfig.CustomTool = writeTool
		editToolConfig.CustomTool = editTool
	}
	lsDesc := `Lists files and directories in a directory.
- path: optional. Omitted = the workspace root. Accepts an absolute path or a path relative to the workspace root (e.g. chapters, setting); relative paths never escape the workspace.
- Use ls before read_file to discover the real directory and file names.

列出目录内容。
- path：可选，省略时列出作品根目录；支持绝对路径，或相对作品根目录的相对路径（如 chapters、setting）；相对路径不会访问作品外内容。
- 先调用 ls 确认真实目录与文件名，再 read_file。`
	globDesc := `Find files matching a glob pattern.
- path: optional base directory to search; omitted = the workspace root. Accepts an absolute path or a path relative to the workspace root.
- pattern: glob expression relative to path, e.g. "**/*.md".

按 glob 模式查找文件。
- path：可选搜索基准目录，省略时以作品根目录为基准；支持绝对路径或相对作品根目录的相对路径。
- pattern：相对 path 的 glob 表达式，如 "**/*.md"。`
	grepDesc := `Search file contents with ripgrep.
- path: optional directory to search; omitted = the workspace root. Accepts an absolute path or a path relative to the workspace root.

用 ripgrep 搜索文件内容。
- path：可选搜索目录，省略时以作品根目录为基准；支持绝对路径或相对作品根目录的相对路径。`
	mwConfig := &filesystemmw.MiddlewareConfig{
		Backend:             backend,
		LsToolConfig:        &filesystemmw.ToolConfig{Desc: &lsDesc},
		ReadFileToolConfig:  readToolConfig,
		GlobToolConfig:      &filesystemmw.ToolConfig{Desc: &globDesc},
		GrepToolConfig:      &filesystemmw.ToolConfig{Desc: &grepDesc},
		WriteFileToolConfig: writeToolConfig,
		EditFileToolConfig:  editToolConfig,
	}
	if streamingShell != nil {
		mwConfig.StreamingShell = streamingShell
	}
	return filesystemmw.New(ctx, mwConfig)
}

func stableWebSearchSchemaAllowed(agentKind string) func(config.ResolvedAgentToolSettings) bool {
	return func(settings config.ResolvedAgentToolSettings) bool {
		if settings.WebSearch {
			return true
		}
		switch agentKind {
		case config.AgentKindIDE, config.AgentKindInteractiveStory, config.AgentKindConfigManager, config.AgentKindAutomation:
			return true
		default:
			return false
		}
	}
}

func configMaxIteration(cfg *config.Config) int {
	if cfg == nil || cfg.MaxIteration <= 0 {
		return unlimitedAgentMaxIterations
	}
	return cfg.MaxIteration
}

func configModelMaxRetries(cfg *config.Config) int {
	if cfg == nil || cfg.ModelMaxRetries < 0 {
		return 5
	}
	return cfg.ModelMaxRetries
}

func configToolResultMaxBytes(cfg *config.Config) int {
	if cfg == nil || cfg.AgentToolResultLimitKB <= 0 {
		return defaultToolResultMaxBytes
	}
	return cfg.AgentToolResultLimitKB * 1024
}

// configToolResultBatchLimitBytes 返回一条消息内工具结果总和上限（字节）。
// 0 表示不设置聚合上限（对应 Claude Code MAX_TOOL_RESULTS_PER_MESSAGE_CHARS=0 时不限）。
func configToolResultBatchLimitBytes(cfg *config.Config) int {
	if cfg == nil || cfg.AgentToolResultBatchLimitKB <= 0 {
		return 0
	}
	return cfg.AgentToolResultBatchLimitKB * 1024
}

// handleUnknownTool 拦截 LLM 调用未知工具的错误，把可读提示作为工具结果回传给模型，
// 引导 Agent 在后续轮次基于该反馈自我修正（例如改用正确的工具名）。
func handleUnknownTool(_ context.Context, name, input string) (string, error) {
	log.Printf("[agent] LLM 调用了不存在的工具 name=%s args=%s", name, input)
	return prompts.UnknownToolMessage(name), nil
}

// BuildToolByName 按工具名重建单个文件系统工具，供前端「重试」端点直接调用。
// 复用与主 Agent 相同的 filesystem 中间件装配路径，保证工具行为一致。
func BuildToolByName(ctx context.Context, cfg *config.Config, workspace, toolName string) (tool.InvokableTool, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, fmt.Errorf("工具名为空")
	}
	localBackend, err := localbk.NewBackend(ctx, &localbk.Config{})
	if err != nil {
		return nil, fmt.Errorf("创建本地文件系统后端失败: %w", err)
	}
	backend := newAgentFilesystemBackend(localBackend, workspace)
	toolSettings := config.ResolveAgentTools(cfg, config.AgentKindIDE)
	mw, err := newFilesystemMiddleware(ctx, backend, newAgentStreamingShell(workspace), toolSettings, workspace)
	if err != nil {
		return nil, fmt.Errorf("创建 filesystem 中间件失败: %w", err)
	}
	if mw == nil {
		return nil, fmt.Errorf("filesystem 中间件未启用（工具开关未开启）")
	}
	_, runCtx, err := mw.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		return nil, fmt.Errorf("装配 filesystem 工具失败: %w", err)
	}
	if runCtx == nil {
		return nil, fmt.Errorf("filesystem 工具上下文为空")
	}
	for _, t := range runCtx.Tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}
		if info != nil && info.Name == toolName {
			invokable, ok := t.(tool.InvokableTool)
			if !ok {
				return nil, fmt.Errorf("工具 %s 不支持同步调用", toolName)
			}
			return invokable, nil
		}
	}
	return nil, fmt.Errorf("未找到工具 %s", toolName)
}
