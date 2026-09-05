package book

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChapterAlias 描述一个可通过 @chN 引用的章节条目，供 Agent 别名索引使用。
type ChapterAlias struct {
	Index      int    `json:"index"`
	Path       string `json:"path"` // 相对 workspace 的斜杠路径
	Title      string `json:"title"`
	Volume     string `json:"volume,omitempty"`
	VolumePath string `json:"volume_path,omitempty"`
}

// ChapterAliases 轻量枚举工作区章节 → 章节序号映射，供 Agent 的 @chN 别名解析使用。
// 只做目录遍历与文件名解析（复用 chapterIndex/chapterVolume 的权威规则），不读取文件内容，
// 因此每次工具调用实时计算的开销可忽略，且章节增删后无需缓存失效。章节路径规则的真源保持在
// book 包，Agent 不重复实现。
func (s *Service) ChapterAliases() ([]ChapterAlias, error) {
	chapterRoot := filepath.Join(s.workspace, "chapters")
	if _, err := os.Stat(chapterRoot); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var aliases []ChapterAlias
	err := filepath.WalkDir(chapterRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		if name != "." && strings.HasPrefix(name, ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !isChapterTextFile(name) {
			return nil
		}
		// 只收集能解析出章节序的文件（含序章 index 0），排除 README.md、说明等无关文本文件。
		if !chapterSortKeyForName(name).ok {
			return nil
		}
		rel, relErr := filepath.Rel(s.workspace, path)
		if relErr != nil {
			return nil
		}
		alias := ChapterAlias{
			Index: chapterIndex(name),
			Path:  filepath.ToSlash(rel),
			Title: chapterDisplayTitle(name),
		}
		alias.Volume, alias.VolumePath = chapterVolume(alias.Path)
		aliases = append(aliases, alias)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(aliases, func(i, j int) bool {
		if aliases[i].Index != aliases[j].Index {
			return aliases[i].Index < aliases[j].Index
		}
		return aliases[i].Path < aliases[j].Path
	})
	return aliases, nil
}
