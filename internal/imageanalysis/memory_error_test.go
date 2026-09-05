package imageanalysis

import (
	"errors"
	"testing"
)

// TestIsMemoryError 验证 isMemoryError 对各类服务端资源错误的识别。
func TestIsMemoryError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"empty", errors.New(""), false},
		{"whitespace", errors.New("   "), false},
		{"bad allocation", errors.New("std::bad_alloc: bad allocation"), true},
		{"bad_alloc", errors.New("bad_alloc thrown during prefill"), true},
		{"out of memory", errors.New("CUDA out of memory"), true},
		{"allocation failed", errors.New("allocation failed at load"), true},
		{"resource exhausted", errors.New("resource exhausted: model overloaded"), true},
		{"not enough memory", errors.New("not enough memory for buffer"), true},
		{"memory error", errors.New("memory error in kernel"), true},
		{"case insensitive", errors.New("BAD ALLOCATION detected"), true},
		{"normal error", errors.New("connection refused"), false},
		{"partial word", errors.New("allocation"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMemoryError(tt.err); got != tt.want {
				t.Fatalf("isMemoryError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
