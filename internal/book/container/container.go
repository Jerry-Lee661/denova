// Package container extracts topic files from ebook container formats —
// Microsoft Compiled HTML Help (CHM, ITSF/LZX) and EPUB — in reading order.
// It only opens the container and surfaces its structure; topic text decoding
// and HTML conversion stay with the caller.
package container

import (
	"fmt"
	"path"
	"strings"

	"github.com/gen2brain/folio/html"
)

// MaxExtractedBytes bounds the total uncompressed topic payload an Archive
// returns, so a crafted container cannot expand without bound. Real reference
// books decompress well above their file size, so this is generous by design.
const MaxExtractedBytes = 256 << 20

// Kind identifies the container format an Archive was opened from.
type Kind int

const (
	KindCHM Kind = iota + 1
	KindEPUB
)

// Topic is one topic file of a container, in spine order: the table of
// contents order first, then any remaining topics in container directory
// order.
type Topic struct {
	// Path is the topic path inside the container, e.g. "01.html".
	Path string
	// HTML holds the raw topic payload, still in its source encoding.
	HTML []byte
}

// TocNode is one entry of a container's table-of-contents tree. EPUB outlines
// come decoded from folio; CHM sitemaps are parsed and decoded by the caller
// because CHM files rarely store them in UTF-8.
type TocNode struct {
	Title    string
	Path     string // normalized topic path key; empty for pure group nodes
	Children []*TocNode
}

// Archive is an opened CHM or EPUB container.
type Archive struct {
	doc  *html.Document
	kind Kind
}

// Open parses data as a CHM or EPUB container. The container kind is
// verified: zip-based formats other than EPUB (Word, PowerPoint, zipped FB2)
// fail with a clear error instead of being read as the wrong format.
func Open(data []byte) (*Archive, error) {
	doc, err := html.Load(data)
	if err != nil {
		return nil, fmt.Errorf("无法解析容器文件: %w", err)
	}
	kind := Kind(0)
	switch doc.Kind() {
	case html.KindCHM:
		kind = KindCHM
	case html.KindEPUB:
		kind = KindEPUB
	}
	if kind == 0 {
		doc.Close()
		return nil, fmt.Errorf("文件不是 CHM 或 EPUB 容器")
	}
	return &Archive{doc: doc, kind: kind}, nil
}

// Kind reports which container format the archive was opened from.
func (a *Archive) Kind() Kind { return a.kind }

// Close releases the container.
func (a *Archive) Close() error {
	return a.doc.Close()
}

// Topics returns the text topics of the container in spine order. Empty
// topics are skipped and the total uncompressed payload is capped at
// MaxExtractedBytes.
func (a *Archive) Topics() ([]Topic, error) {
	spine := a.doc.Spine()
	topics := make([]Topic, 0, len(spine))
	total := 0
	for _, item := range spine {
		payload, err := a.doc.Read(item.Path)
		if err != nil {
			return nil, fmt.Errorf("读取容器主题 %s 失败: %w", item.Path, err)
		}
		if len(payload) == 0 {
			continue
		}
		total += len(payload)
		if total > MaxExtractedBytes {
			return nil, fmt.Errorf("容器解压后内容超过 %d 字节上限", MaxExtractedBytes)
		}
		topics = append(topics, Topic{Path: item.Path, HTML: payload})
	}
	return topics, nil
}

// Sitemap returns the raw bytes of the CHM container's first .hhc table of
// contents, still in its source encoding, and whether one exists. Callers
// decode it themselves because CHM files rarely store the sitemap in UTF-8.
// EPUB archives have no .hhc sitemap; use TocTree instead.
func (a *Archive) Sitemap() ([]byte, bool) {
	if a.kind != KindCHM {
		return nil, false
	}
	for _, item := range a.doc.Manifest() {
		if strings.EqualFold(path.Ext(item.Path), ".hhc") {
			raw, err := a.doc.Read(item.Path)
			if err != nil || len(raw) == 0 {
				return nil, false
			}
			return raw, true
		}
	}
	return nil, false
}

// TocTree returns the container's decoded table-of-contents tree. For EPUB
// this is folio's parsed outline (nav document, NCX fallback, or headings);
// CHM callers parse Sitemap themselves because folio does not decode the
// GBK sitemaps Chinese CHMs carry. The second result reports whether a
// table of contents exists.
func (a *Archive) TocTree() ([]*TocNode, bool) {
	if a.kind != KindEPUB {
		return nil, false
	}
	outline := a.doc.Outline()
	if len(outline) == 0 {
		return nil, false
	}
	return convertOutline(outline), true
}

func convertOutline(items []html.Outline) []*TocNode {
	out := make([]*TocNode, 0, len(items))
	for _, item := range items {
		node := &TocNode{Title: strings.TrimSpace(item.Title)}
		if !strings.Contains(item.Path, "://") {
			// External links are not topics of this container.
			node.Path = item.Path
		}
		node.Children = convertOutline(item.Children)
		out = append(out, node)
	}
	return out
}
