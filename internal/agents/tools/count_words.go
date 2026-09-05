package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"denova/config"
	agent "github.com/alfredxw/denova/agent"
)

const countWordsToolName = "count_words"

const countWordsToolDescription = `Count characters and words in a workspace file or inline text. Returns multiple metrics so the caller can pick the one that matches the writing plan.

Primary metric for Chinese fiction: chinese_chars (CJK unified ideographs, one per character).

- Provide file_path to count a file inside the workspace, or text to count inline content. Provide exactly one.
- chinese_chars: number of CJK unified ideographs (U+4E00-U+9FFF, U+3400-U+4DBF, U+F900-U+FAFF, and CJK extension ranges). This is the default "中文字数" metric.
- chars_no_space: total characters excluding all whitespace (spaces, tabs, newlines). Includes CJK, punctuation, and Latin letters.
- chars_with_space: total characters including whitespace.
- lines: number of lines (newline-separated).
- non_cjk_words: space-separated word count of non-CJK text segments (useful for mixed-language content).`

type countWordsInput struct {
	FilePath string `json:"file_path,omitempty" jsonschema:"description=Path of a workspace file to count. Relative paths resolve against the current Project workspace. Mutually exclusive with text."`
	Text     string `json:"text,omitempty" jsonschema:"description=Inline text to count. Mutually exclusive with file_path."`
}

type countWordsResult struct {
	Schema         string `json:"schema"`
	Source         string `json:"source"`
	ChineseChars   int    `json:"chinese_chars"`
	CharsNoSpace   int    `json:"chars_no_space"`
	CharsWithSpace int    `json:"chars_with_space"`
	Lines          int    `json:"lines"`
	NonCJKWords    int    `json:"non_cjk_words"`
}

// CountWords returns the count_words tool definition. Counting a file reads
// workspace content, so the tool is gated by the filesystem_read capability.
func CountWords(workspace string) (agent.ToolDefinition, error) {
	workspace = strings.TrimSpace(workspace)
	tool, err := agent.InferTool(countWordsToolName, countWordsToolDescription, func(ctx context.Context, input countWordsInput) (agent.ToolResult, error) {
		hasFile := strings.TrimSpace(input.FilePath) != ""
		hasText := len(input.Text) > 0
		if hasFile == hasText {
			return agent.ToolResult{}, errors.New("provide exactly one of file_path or text")
		}

		var content string
		var source string
		if hasFile {
			path := strings.TrimSpace(input.FilePath)
			if !filepath.IsAbs(path) {
				if workspace == "" {
					return agent.ToolResult{}, errors.New("no workspace is active; provide an absolute file path or inline text")
				}
				path = filepath.Join(workspace, filepath.FromSlash(path))
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					return agent.ToolResult{}, fmt.Errorf("file not found: %s", path)
				}
				return agent.ToolResult{}, fmt.Errorf("stat file for count_words: %w", statErr)
			}
			if info.IsDir() {
				return agent.ToolResult{}, fmt.Errorf("%s is a directory, not a file: list its contents with glob instead", path)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return agent.ToolResult{}, fmt.Errorf("read file for count_words: %w", readErr)
			}
			content = string(data)
			source = "workspace_file"
		} else {
			content = input.Text
			source = "inline_text"
		}

		result := countTextMetrics(content)
		result.Schema = "count_words.v1"
		result.Source = source

		out, err := json.Marshal(result)
		if err != nil {
			return agent.ToolResult{}, fmt.Errorf("serialize count_words result: %w", err)
		}
		return agent.TextToolResult(string(out)), nil
	})
	if err != nil {
		return agent.ToolDefinition{}, fmt.Errorf("create count_words tool: %w", err)
	}
	return agent.ToolDefinition{
		Tool:       tool,
		Descriptor: BoundedReadDescriptor(agent.ToolSourceOther, config.AgentToolFilesystemRead),
	}, nil
}

func countTextMetrics(text string) countWordsResult {
	var chineseChars, charsNoSpace, charsWithSpace, nonCJKWords int
	lines := 1
	inNonCJKWord := false

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size

		charsWithSpace++

		if r == '\n' {
			lines++
			if inNonCJKWord {
				nonCJKWords++
				inNonCJKWord = false
			}
			continue
		}

		if unicode.IsSpace(r) {
			if inNonCJKWord {
				nonCJKWords++
				inNonCJKWord = false
			}
			continue
		}

		charsNoSpace++

		if isCJKRune(r) {
			chineseChars++
			if inNonCJKWord {
				nonCJKWords++
				inNonCJKWord = false
			}
		} else {
			inNonCJKWord = true
		}
	}
	if inNonCJKWord {
		nonCJKWords++
	}
	if len(text) == 0 {
		lines = 0
	}

	return countWordsResult{
		ChineseChars:   chineseChars,
		CharsNoSpace:   charsNoSpace,
		CharsWithSpace: charsWithSpace,
		Lines:          lines,
		NonCJKWords:    nonCJKWords,
	}
}

func isCJKRune(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility Ideographs
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK Extension B
		(r >= 0x2A700 && r <= 0x2B73F) || // CJK Extension C
		(r >= 0x2B740 && r <= 0x2B81F) || // CJK Extension D
		(r >= 0x2B820 && r <= 0x2CEAF) || // CJK Extension E
		(r >= 0x2CEB0 && r <= 0x2EBEF) || // CJK Extension F
		(r >= 0x30000 && r <= 0x3134F) // CJK Extension G
}
