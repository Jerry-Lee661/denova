package book

import (
	"fmt"
	"log"
	"path"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"

	"denova/internal/book/container"
)

// extractContainerChapters converts a .chm or .epub upload into chapters that
// mirror the container's own table-of-contents tree. Denova has two content
// levels, so the tree folds as: level-1 entries become volumes, level-3
// entries become chapters titled "level-2 · level-3" (level-2 entries with
// their own page keep a chapter of that page), and everything deeper is
// concatenated into its level-3 ancestor's chapter with hierarchical section
// headings. Topics outside the table of contents are grouped by their first
// path segment into a volume of that name. Titles come from the decoded table
// of contents (CHM files rarely store it in UTF-8), falling back to each
// topic's <title>.
func extractContainerChapters(data []byte) ([]parsedNovelChapter, error) {
	archive, err := container.Open(data)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	topics, err := archive.Topics()
	if err != nil {
		return nil, err
	}
	if len(topics) == 0 {
		return nil, fmt.Errorf("文件内容为空")
	}
	var toc []*container.TocNode
	var tocOK bool
	switch archive.Kind() {
	case container.KindCHM:
		toc = parseCHMSitemapTree(archive)
		tocOK = true
	case container.KindEPUB:
		toc, tocOK = archive.TocTree()
		normalizeTocPaths(toc)
	}

	converter := htmltomarkdown.NewConverter("", true, nil)
	bodies := make(map[string]string, len(topics))
	fallbackTitles := make(map[string]string, len(topics))
	for _, topic := range topics {
		decoded, decodeErr := decodeNovelTextBytes(topic.HTML)
		if decodeErr != nil {
			return nil, fmt.Errorf("只支持 UTF-8、UTF-16 或 GB18030 编码的容器主题 %s", topic.Path)
		}
		htmlTitle, body := convertTopicToMarkdown(converter, decoded)
		key := chmPathKey(topic.Path)
		if _, seen := bodies[key]; !seen {
			bodies[key] = body
			fallbackTitles[key] = htmlTitle
		}
	}
	log.Printf("[novel-import] container extracted kind=%v topics=%d toc=%v", archive.Kind(), len(topics), tocOK)

	used := map[string]bool{}
	inTree := map[string]bool{}
	var chapters []parsedNovelChapter

	ownBody := func(node *container.TocNode) string {
		if node.Path == "" || used[node.Path] {
			return ""
		}
		body := bodies[node.Path]
		if strings.TrimSpace(body) == "" {
			return ""
		}
		used[node.Path] = true
		return body
	}

	// renderChildren appends every descendant page, headed by its title at a
	// heading depth that follows the tree depth (## for direct children).
	var renderChildren func(children []*container.TocNode, depth int, parts *[]string)
	renderChildren = func(children []*container.TocNode, depth int, parts *[]string) {
		for _, child := range children {
			if child.Path != "" {
				inTree[child.Path] = true
			}
			if body := ownBody(child); body != "" {
				if child.Title != "" && !strings.HasPrefix(body, "#") {
					level := depth
					if level > 6 {
						level = 6
					}
					*parts = append(*parts, strings.Repeat("#", level)+" "+child.Title)
				}
				*parts = append(*parts, body)
			}
			renderChildren(child.Children, depth+1, parts)
		}
	}

	appendChapter := func(node *container.TocNode, volume, titleOverride string) {
		if node.Path != "" {
			inTree[node.Path] = true
		}
		parts := []string{}
		if body := ownBody(node); body != "" {
			parts = append(parts, body)
		}
		renderChildren(node.Children, 2, &parts)
		content := strings.TrimSpace(strings.Join(parts, "\n\n"))
		if content == "" {
			return
		}
		title := firstNonEmpty(titleOverride, node.Title, fallbackTitles[node.Path], chmPathTitle(node.Path))
		chapters = append(chapters, parsedNovelChapter{
			NovelImportChapter: NovelImportChapter{Title: title, Volume: volume},
			Content:            content + "\n",
		})
	}

	for _, top := range toc {
		if len(top.Children) == 0 {
			appendChapter(top, "", "")
			continue
		}
		if body := ownBody(top); body != "" {
			chapters = append(chapters, parsedNovelChapter{
				NovelImportChapter: NovelImportChapter{Title: top.Title, Volume: top.Title},
				Content:            body + "\n",
			})
		}
		for _, second := range top.Children {
			if len(second.Children) == 0 {
				appendChapter(second, top.Title, "")
				continue
			}
			if body := ownBody(second); body != "" {
				chapters = append(chapters, parsedNovelChapter{
					NovelImportChapter: NovelImportChapter{Title: second.Title, Volume: top.Title},
					Content:            body + "\n",
				})
			}
			for _, third := range second.Children {
				title := third.Title
				if second.Title != "" && title != "" {
					title = second.Title + " · " + title
				}
				appendChapter(third, top.Title, title)
			}
		}
	}

	// Topics the table of contents never mentions keep their spine order.
	// CHM folders carry content groupings, so unlisted topics are grouped by
	// their first path segment; EPUB folders are layout artifacts and stay
	// unfiled.
	orphanVolume := func(topicPath string) string {
		if archive.Kind() != container.KindCHM {
			return ""
		}
		normalized := strings.ReplaceAll(topicPath, "\\", "/")
		if slash := strings.Index(normalized, "/"); slash > 0 {
			return normalized[:slash]
		}
		return ""
	}
	for _, topic := range topics {
		key := chmPathKey(topic.Path)
		if used[key] || inTree[key] {
			continue
		}
		body := strings.TrimSpace(bodies[key])
		if body == "" {
			continue
		}
		used[key] = true
		title := firstNonEmpty(fallbackTitles[key], chmPathTitle(topic.Path))
		chapters = append(chapters, parsedNovelChapter{
			NovelImportChapter: NovelImportChapter{Title: title, Volume: orphanVolume(topic.Path)},
			Content:            body + "\n",
		})
	}

	if len(chapters) == 0 {
		return nil, fmt.Errorf("文件内容为空")
	}
	return chapters, nil
}

// convertTopicToMarkdown converts one topic document into Markdown, returning
// the topic's <title> text and the converted body. The head is dropped so
// <title> metadata stays out of the chapter text.
func convertTopicToMarkdown(converter *htmltomarkdown.Converter, doc string) (string, string) {
	parsed, err := goquery.NewDocumentFromReader(strings.NewReader(doc))
	if err != nil {
		markdown, err := converter.ConvertString(doc)
		if err != nil {
			return "", ""
		}
		return "", strings.TrimSpace(markdown)
	}
	title := strings.TrimSpace(parsed.Find("title").First().Text())
	bodySelection := parsed.Find("body")
	if bodySelection.Length() == 0 {
		markdown, err := converter.ConvertString(doc)
		if err != nil {
			return title, ""
		}
		return title, strings.TrimSpace(markdown)
	}
	body, err := bodySelection.Html()
	if err != nil {
		return title, ""
	}
	markdown, err := converter.ConvertString(body)
	if err != nil {
		return title, ""
	}
	return title, strings.TrimSpace(markdown)
}

// parseCHMSitemapTree walks the CHM container's .hhc table of contents into a
// decoded node tree. Classic .hhc files close <li> implicitly, so a nested
// list can be a sibling <ul> grouping under the nearest preceding item; the
// also-common <li><object/><ul/></li> style nests it inside the item. Both
// shapes are handled. A missing or malformed sitemap yields an empty root.
func parseCHMSitemapTree(archive *container.Archive) []*container.TocNode {
	raw, ok := archive.Sitemap()
	if !ok {
		return nil
	}
	decoded, err := decodeNovelTextBytes(raw)
	if err != nil {
		log.Printf("[novel-import] chm sitemap decode failed err=%v", err)
		return nil
	}
	parsed, err := goquery.NewDocumentFromReader(strings.NewReader(decoded))
	if err != nil {
		return nil
	}

	var walkList func(sel *goquery.Selection) []*container.TocNode
	walkList = func(sel *goquery.Selection) []*container.TocNode {
		var out []*container.TocNode
		var last *container.TocNode
		sel.Children().Each(func(_ int, n *goquery.Selection) {
			switch goquery.NodeName(n) {
			case "li":
				title, local := chmSitemapObject(n)
				node := &container.TocNode{Title: title}
				if local != "" {
					node.Path = chmPathKey(local)
				}
				n.Children().Each(func(_ int, c *goquery.Selection) {
					if goquery.NodeName(c) == "ul" {
						node.Children = append(node.Children, walkList(c)...)
					}
				})
				out = append(out, node)
				last = node
			case "ul", "ol":
				kids := walkList(n)
				if last != nil {
					last.Children = append(last.Children, kids...)
				} else {
					out = append(out, kids...)
				}
			}
		})
		return out
	}
	var root []*container.TocNode
	parsed.Find("body").Each(func(_ int, body *goquery.Selection) {
		root = append(root, walkList(body)...)
	})
	return root
}

// normalizeTocPaths folds outline paths into the same normalized space the
// topic lookup keys use (folio keeps the original entry casing).
func normalizeTocPaths(nodes []*container.TocNode) {
	for _, node := range nodes {
		if node.Path != "" {
			node.Path = chmPathKey(node.Path)
		}
		normalizeTocPaths(node.Children)
	}
}

// chmSitemapObject reads the Name and Local params of the sitemap object of
// one list item, e.g. Name="第一章" Local="01.htm".
func chmSitemapObject(li *goquery.Selection) (string, string) {
	title, local := "", ""
	li.Children().Each(func(_ int, n *goquery.Selection) {
		if goquery.NodeName(n) != "object" {
			return
		}
		n.Children().Each(func(_ int, p *goquery.Selection) {
			if goquery.NodeName(p) != "param" {
				return
			}
			name, _ := p.Attr("name")
			value, _ := p.Attr("value")
			switch strings.ToLower(name) {
			case "name":
				if title == "" {
					title = strings.TrimSpace(value)
				}
			case "local":
				if local == "" {
					local = value
				}
			}
		})
	})
	return title, local
}

// chmPathKey normalizes a sitemap Local target to look it up against topic
// paths: forward slashes, no leading slash, no #fragment, case folded.
func chmPathKey(local string) string {
	local = strings.ReplaceAll(local, "\\", "/")
	local = strings.TrimPrefix(local, "/")
	if at := strings.Index(local, "#"); at >= 0 {
		local = local[:at]
	}
	return strings.ToLower(local)
}

// chmPathTitle derives a fallback chapter title from a topic path.
func chmPathTitle(topicPath string) string {
	base := path.Base(strings.ReplaceAll(topicPath, "\\", "/"))
	return strings.TrimSuffix(base, path.Ext(base))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
