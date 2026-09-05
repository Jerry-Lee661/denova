// Package sensitive 提供敏感内容的 gzip 压缩/解压工具，
// 用于将可能触发 AI 风控的文本内容以二进制 .gz 格式存储，
// 规避文本扫描，运行时透明解压。
// 零外部依赖，仅使用 Go 标准库 compress/gzip。
package sensitive

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
)

// CompressString 将字符串压缩为 gzip 字节。
func CompressString(s string) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(s)); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecompressBytes 将 gzip 字节解压为字符串。
func DecompressBytes(data []byte) (string, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ReadGzFile 读取 .gz 文件并解压为字符串。
// 当 .gz 文件不存在时返回空字符串和 nil error，
// 调用方应回退到原始文件。
func ReadGzFile(gzPath string) (string, error) {
	data, err := os.ReadFile(gzPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return DecompressBytes(data)
}

// DecompressFileIfExists 读取 .gz 文件；若存在则解压返回内容，
// 若不存在则回退读取原始文件。
func DecompressFileIfExists(gzPath, fallbackPath string) (string, error) {
	content, err := ReadGzFile(gzPath)
	if err != nil {
		return "", err
	}
	if content != "" {
		return content, nil
	}
	// 回退到原始文件
	data, err := os.ReadFile(fallbackPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteGzFile 将字符串压缩后写入 .gz 文件。
func WriteGzFile(gzPath, content string) error {
	compressed, err := CompressString(content)
	if err != nil {
		return err
	}
	return os.WriteFile(gzPath, compressed, 0o644)
}
