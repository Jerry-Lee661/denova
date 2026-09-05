package agent

import (
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// 尾部切开规则（M8 协议完整性守卫）：
//
// 与 token 上限不同，它是「更硬」的约束——当按尾部裁剪消息以适配预算时，
// 被裁掉的消息不能把一次工具调用与其结果拆开，也不能把同一条消息的思考片段切散。
// 因为工具调用与结果在模型侧是结构化配对（ToolCallID 对应），跨轮回填时若只留其一，
// 下一轮模型会看到悬空的调用或无果的结果；思考片段属于单条消息，天然随消息整体保留。
//
// 实现策略：把每条消息划分到「协议单元」——工具调用（assistant）与其全部结果（tool）
// 通过 ToolCallID 归并为同一单元，其余消息为单元素单元。按头部顺序贪心累积单元，
// 累积到下一个单元会超出预算即停止；首个单元即使超预算也保留（宁可多保留）。
// 返回按原始顺序排列的保留前缀。

// messageBlock 表示一个不可再分的协议块：单元素块为单条消息，多元素块为一组
// 通过 ToolCallID 归并的工具调用与结果。按最小索引升序排列。
type messageBlock struct {
	indexes []int
}

// CutMessagesToFitBudget 从尾部裁剪消息使其适配 maxTokens 估算 token，
// 保证工具调用/结果配对完整、单条消息内容不被拆分。
// 当 maxTokens<=0 或输入为空时原样返回；保留量可能超过预算（协议优先）。
func CutMessagesToFitBudget(messages []*schema.Message, maxTokens int) []*schema.Message {
	if maxTokens <= 0 || len(messages) == 0 {
		return messages
	}
	blocks := buildCutBlocks(messages)
	var kept [][]int
	used := 0
	for _, block := range blocks {
		blockTokens := 0
		for _, idx := range block.indexes {
			blockTokens += estimateMessageTokens(messages[idx])
		}
		if len(kept) > 0 && used+blockTokens > maxTokens {
			break
		}
		used += blockTokens
		kept = append(kept, block.indexes)
	}
	return assembleKeptPrefix(messages, kept)
}

// assembleKeptPrefix 将选中的单元索引合并为按原始顺序排列的保留前缀。
func assembleKeptPrefix(messages []*schema.Message, kept [][]int) []*schema.Message {
	indexSet := make(map[int]struct{}, len(kept))
	for _, unit := range kept {
		for _, idx := range unit {
			indexSet[idx] = struct{}{}
		}
	}
	order := make([]int, 0, len(indexSet))
	for idx := range indexSet {
		order = append(order, idx)
	}
	sort.Ints(order)
	result := make([]*schema.Message, 0, len(order))
	for _, idx := range order {
		if idx >= 0 && idx < len(messages) && messages[idx] != nil {
			result = append(result, messages[idx])
		}
	}
	return result
}

// buildCutBlocks 将消息划分为协议块：工具调用（assistant）与其全部结果（tool）
// 通过 ToolCallID 归并为同一块，其余消息为单元素块。按最小索引升序返回。
func buildCutBlocks(messages []*schema.Message) []messageBlock {
	parent := make([]int, len(messages))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	join := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// 记录每个 ToolCallID 所属的 assistant 消息索引。
	callOwner := make(map[string]int)
	for i, msg := range messages {
		if msg == nil || msg.Role != schema.Assistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			if strings.TrimSpace(call.ID) != "" {
				callOwner[call.ID] = i
			}
		}
	}

	// 将每个 tool 结果与其调用方 assistant 消息归并。
	for i, msg := range messages {
		if msg == nil || msg.Role != schema.Tool {
			continue
		}
		if owner, ok := callOwner[strings.TrimSpace(msg.ToolCallID)]; ok && owner != i {
			join(owner, i)
		}
	}

	groups := make(map[int][]int)
	for i := range messages {
		if messages[i] == nil {
			continue
		}
		root := find(i)
		groups[root] = append(groups[root], i)
	}

	blocks := make([]messageBlock, 0, len(groups))
	for _, group := range groups {
		sort.Ints(group)
		blocks = append(blocks, messageBlock{indexes: group})
	}
	sort.Slice(blocks, func(a, b int) bool {
		return blocks[a].indexes[0] < blocks[b].indexes[0]
	})
	return blocks
}
