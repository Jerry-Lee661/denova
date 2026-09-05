package session

import (
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
)

// 逻辑链（副指针图）相关常量与操作。
//
// Denova 的会话记录是只追加文件，但逻辑上不是简单数组：每条消息通过 prevIdx
// 记录其在逻辑链中的上一条消息，branchID 记录所属分支。活动分支由 activeBranch
// / activeLast 追踪，加载时从活动分支末端沿 prevIdx 回溯重建真正要继续的逻辑链。
//
// 本文件所有方法都保持 append-only：改变结构只追加 chain_link / fork 记录，
// 不改旧行。这保证任何压缩机制都不破坏磁盘上的原始记录。

const (
	// historyTypeChainLink 记录一条消息的父指针与分支归属。
	historyTypeChainLink = "chain_link"
	// historyTypeFork 记录一次分支分叉（fork）的活跃状态。
	historyTypeFork = "fork"
)

// chainLinkRecord 持久化单条消息的父指针与分支标识。
type chainLinkRecord struct {
	Type         string `json:"type"`
	MessageIndex int    `json:"message_index"`
	PrevIndex    int    `json:"prev_index"`
	BranchID     string `json:"branch_id,omitempty"`
}

// forkRecord 持久化当前活动分支指针（最后一次写入者生效）。
type forkRecord struct {
	Type         string    `json:"type"`
	ActiveBranch string    `json:"active_branch,omitempty"`
	ActiveLast   int       `json:"active_last"`
	ForkPoint    int       `json:"fork_point"`
	CreatedAt    time.Time `json:"created_at"`
}

// ensureChainLocked 确保 prevIdx/branchID 数组长度与 messages 一致。
// 缺失部分按线性链默认填充（prevIdx = i-1，空分支），实现旧记录向后兼容。
func (s *Session) ensureChainLocked() {
	for len(s.prevIdx) < len(s.messages) {
		prev := len(s.prevIdx) - 1
		if prev < 0 {
			prev = -1
		}
		s.prevIdx = append(s.prevIdx, prev)
		s.branchID = append(s.branchID, "")
	}
}

// LinkCurrent 为刚追加的末尾消息建立父指针。
// prev 为该消息在逻辑链中的上一条消息索引；-1 表示根（无前驱）。
// branch 为该消息所属分支标识，空字符串表示默认线性分支。
// 若此前调用过 Fork，则新消息自动归属该分支：第一条消息以分叉点为前驱，
// 后续消息以分支末端为前驱。
// 调用方需持有 s.mu。
func (s *Session) LinkCurrent(prev int, branch string) {
	s.ensureChainLocked()
	idx := len(s.messages) - 1
	if prev < 0 {
		prev = idx - 1
	}
	// 若存在未消费的分叉状态，新消息归属该分支。
	if s.activeBranch != "" && s.forkPoint >= 0 {
		if s.branchTip >= 0 {
			// 分支已有消息：新消息从分支末端链接。
			prev = s.branchTip
		} else {
			// 分支第一条消息：前驱为分叉点。
			prev = s.forkPoint
		}
		branch = s.activeBranch
	}
	s.setPrevAt(idx, prev, branch)
	s.activeLast = idx
	s.branchTip = idx
}

// setPrevAt 显式设置指定索引消息的父指针与分支。调用方需持有 s.mu。
func (s *Session) setPrevAt(idx, prev int, branch string) {
	if idx < 0 || idx >= len(s.messages) {
		return
	}
	s.ensureChainLocked()
	for len(s.prevIdx) <= idx {
		prevIdx := len(s.prevIdx) - 1
		if prevIdx < 0 {
			prevIdx = -1
		}
		s.prevIdx = append(s.prevIdx, prevIdx)
		s.branchID = append(s.branchID, "")
	}
	s.prevIdx[idx] = prev
	s.branchID[idx] = branch
}

// LinkMessage 显式设置末尾消息的父指针与分支，并持久化 chain_link 记录。
// 用于 fork 后第一条消息等需要自定义前驱的场景。
func (s *Session) LinkMessage(prev int, branch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureChainLocked()
	idx := len(s.messages) - 1
	if idx < 0 {
		return fmt.Errorf("会话无消息可链接")
	}
	s.setPrevAt(idx, prev, branch)
	s.records = append(s.records, historyRecord{
		kind:         historyTypeChainLink,
		messageIndex: idx,
		prevIndex:    prev,
		branchID:     branch,
		createdAt:    time.Now().UTC(),
	})
	if err := s.persistLocked(); err != nil {
		return err
	}
	return nil
}

// Fork 从指定消息索引分叉出一条新活动分支。
// branch 为新分支标识；from 为分叉点（新分支第一条消息的前驱）。
// fork 不接管原记录文件、不继承原 work tree 所有权——调用方负责迁移外置决定。
// 调用 Fork 后追加的第一条消息将把 from 作为其前驱并归属分支 branch；
// 之后追加的消息继续沿该分支链接。
func (s *Session) Fork(branch, from string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := len(s.messages) - 1
	if idx < 0 {
		return fmt.Errorf("会话无消息可分叉")
	}
	s.activeBranch = branch
	s.forkPoint = parseChainIndex(from, idx)
	// 分支末端初始为分叉点：第一条消息以分叉点为前驱，后续消息从分支末端链接。
	s.branchTip = -1
	s.records = append(s.records, historyRecord{
		kind:      historyTypeFork,
		fork:      &forkRecord{Type: historyTypeFork, ActiveBranch: branch, ForkPoint: s.forkPoint, CreatedAt: time.Now().UTC()},
		createdAt: time.Now().UTC(),
	})
	return s.persistLocked()
}

// ActiveBranch 返回当前活动分支标识（空字符串表示默认线性分支）。
// 用于 resume 运行时状态记录：work tree 所有权跟随活动分支。
func (s *Session) ActiveBranch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeBranch
}

// ActiveChain 沿活动分支从末端回溯，返回逻辑链顺序（从根到末端）的索引切片。
// 用于重建真正要继续的逻辑链；未活动的分支消息不包含在内。
func (s *Session) ActiveChain() []int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureChainLocked()
	if s.activeLast < 0 || s.activeLast >= len(s.prevIdx) {
		return nil
	}
	var chain []int
	visited := make(map[int]bool)
	for idx := s.activeLast; idx >= 0 && !visited[idx]; idx = s.prevIdx[idx] {
		visited[idx] = true
		chain = append(chain, idx)
	}
	// 回溯得到的是逆序，反转回从根到末端。
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// BoundaryIndex 返回压缩边界：PrepareMessages / GetEffectiveMessages 从此之后投影。
func (s *Session) BoundaryIndex() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveBoundaryLocked()
}

// SetBoundary 前进压缩边界（全量压缩写入新边界时调用）。只设置不持久化，
// 由调用方在追加 compaction 记录后统一持久化。
func (s *Session) SetBoundary(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index > s.clearAfterIndex && index <= len(s.messages) {
		s.boundaryIndex = index
	}
}

// effectiveBoundaryLocked 返回当前投影边界：优先使用显式 boundaryIndex，
// 未设置时回退到 clearAfterIndex。调用方需持有 s.mu。
func (s *Session) effectiveBoundaryLocked() int {
	if s.boundaryIndex > s.clearAfterIndex {
		return s.boundaryIndex
	}
	return s.clearAfterIndex
}

// parseChainIndex 将分支消息的字符串前驱索引解析为整数；越界时返回 idx。
func parseChainIndex(from string, idx int) int {
	if from == "" {
		return -1
	}
	n := 0
	for _, r := range from {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		} else {
			break
		}
	}
	if n < 0 || n > idx {
		return idx
	}
	return n
}

// GetEffectiveMessages 返回压缩边界之后（沿活动分支）的 Agent 有效上下文。
// 与线性版本不同：先取活动分支末端，再沿 prevIdx 回溯收集逻辑链，最后按边界裁剪。
func (s *Session) getEffectiveMessagesLocked() []*schema.Message {
	s.ensureChainLocked()
	boundary := s.effectiveBoundaryLocked()

	chain := s.activeChainLocked()
	inChain := make(map[int]bool, len(chain))
	for _, idx := range chain {
		inChain[idx] = true
	}

	result := make([]*schema.Message, 0, len(chain))
	for _, idx := range chain {
		if idx < boundary {
			continue
		}
		if idx >= len(s.messages) {
			continue
		}
		// 会话逻辑隐藏区间 [hiddenStart, hiddenEnd)：区间内的消息不投影给模型，
		// 但区间之后新增的消息（索引 >= hiddenEnd）正常投影。
		if s.hiddenStart >= 0 && idx >= s.hiddenStart && idx < s.hiddenEnd {
			continue
		}
		msg := s.messages[idx]
		if msg != nil {
			result = append(result, msg)
		}
	}
	return result
}

// activeChainLocked 与 ActiveChain 逻辑一致，调用方需持有 s.mu。
func (s *Session) activeChainLocked() []int {
	if s.activeLast < 0 || s.activeLast >= len(s.prevIdx) {
		return nil
	}
	var chain []int
	visited := make(map[int]bool)
	for idx := s.activeLast; idx >= 0 && !visited[idx]; idx = s.prevIdx[idx] {
		visited[idx] = true
		chain = append(chain, idx)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}
