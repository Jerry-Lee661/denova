// Package searchindex maintains a rebuildable hybrid retrieval index over one
// book workspace. It is derived data: the workspace files remain the only
// source of truth, the index directory can be deleted at any time, and a
// missing or stale index is rebuilt in place. The keyword channel (BM25 over
// CJK n-gram and ASCII word tokens) needs no external service; the semantic
// channel additionally embeds chunks through a configurable OpenAI-compatible
// endpoint and gracefully degrades to keyword-only ranking.
package searchindex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"denova/internal/book"
)

const (
	indexSchemaVersion  = 1
	chunkerVersion      = 1
	maxIndexedFileBytes = 2 << 20

	// DefaultLimit is the default number of returned results.
	DefaultLimit = 8
	// MaxLimit bounds one search response.
	MaxLimit = 20

	manifestFileName = "manifest.json"
	chunksFileName   = "chunks.jsonl"
	vectorsFileName  = "vectors.f32"
	cacheFileName    = "embed-cache.bin"

	embedCacheMagic = "EGC1"

	// DefaultIndexDirName is the index directory name inside the Project Store.
	DefaultIndexDirName = "search-index"
)

var indexableExtensions = map[string]struct{}{
	".md": {}, ".txt": {}, ".csv": {}, ".json": {}, ".toml": {}, ".yaml": {}, ".yml": {},
}

// loreStorePrefix is excluded because the lore store keeps its own catalog and
// read tools; its items.json would otherwise be indexed as raw JSON noise.
const loreStorePrefix = "setting/lore/"

// Embedder is the semantic channel. *embedding.Client satisfies it. A nil
// Embedder keeps the index and ranking keyword-only.
type Embedder interface {
	Model() string
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
}

// Options selects the workspace to index and the rebuildable index location.
type Options struct {
	Workspace string
	IndexDir  string
	Embedder  Embedder
}

// Result is one ranked retrieval hit.
type Result struct {
	Path  string
	Title string
	Start int // rune offset of the chunk within the file
	Score float64
	Text  string
}

type chunk struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Title string `json:"title"`
	Start int    `json:"start"`
	Text  string `json:"text"`
}

type manifest struct {
	SchemaVersion  int    `json:"schema_version"`
	ChunkerVersion int    `json:"chunker_version"`
	EmbedModel     string `json:"embed_model"`
	EmbedDim       int    `json:"embed_dim"`
	FileCount      int    `json:"file_count"`
	Fingerprint    string `json:"fingerprint"`
}

type fileEntry struct {
	rel     string
	abs     string
	size    int64
	modTime int64
}

type sourceIndex struct {
	chunks  []chunk
	vectors [][]float32
	bm25    *bm25Index
	// model is the embedder model the stored vectors belong to ("" for
	// keyword-only builds).
	model string
}

// DefaultIndexDir returns the rebuildable index location: inside the Project
// Store when available, otherwise a hidden directory next to the workspace.
func DefaultIndexDir(workspace, projectStoreRoot string) string {
	if root := strings.TrimSpace(projectStoreRoot); root != "" {
		return filepath.Join(root, DefaultIndexDirName)
	}
	return filepath.Join(workspace, "."+DefaultIndexDirName)
}

var (
	buildLocks  sync.Map // indexDir -> *sync.Mutex
	memoryCache sync.Map // indexDir -> cacheEntry
)

type cacheEntry struct {
	fingerprint string
	model       string
	index       *sourceIndex
}

// Search runs hybrid retrieval: BM25 and (when configured) vector ranking are
// fused with reciprocal rank fusion.
func Search(ctx context.Context, opts Options, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("search query is empty")
	}
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, errors.New("search requires a workspace")
	}
	if strings.TrimSpace(opts.IndexDir) == "" {
		return nil, errors.New("search requires an index directory")
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	idx, err := ensureIndex(ctx, opts)
	if err != nil {
		return nil, err
	}
	lists := make([][]scored, 0, 2)
	if keyword := idx.bm25.search(query, limit*4); len(keyword) > 0 {
		lists = append(lists, keyword)
	}
	if opts.Embedder != nil && len(idx.vectors) > 0 {
		queryVector, embedErr := opts.Embedder.EmbedQuery(ctx, query)
		if embedErr != nil {
			slog.Warn("searchindex: query embedding failed, ranking keyword-only", "error", embedErr)
		} else if semantic := searchVectors(idx.vectors, normalizeVector(queryVector), limit*4); len(semantic) > 0 {
			lists = append(lists, semantic)
		}
	}
	ranked := fuse(lists, limit)
	results := make([]Result, 0, len(ranked))
	for _, item := range ranked {
		source := idx.chunks[item.doc]
		results = append(results, Result{
			Path: source.Path, Title: source.Title, Start: source.Start,
			Score: item.score, Text: source.Text,
		})
	}
	return results, nil
}

func ensureIndex(ctx context.Context, opts Options) (*sourceIndex, error) {
	workspace, err := filepath.Abs(strings.TrimSpace(opts.Workspace))
	if err != nil {
		return nil, err
	}
	indexDir, err := filepath.Abs(strings.TrimSpace(opts.IndexDir))
	if err != nil {
		return nil, err
	}

	lockValue, _ := buildLocks.LoadOrStore(indexDir, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	files, fingerprint := scanWorkspace(workspace)
	embedModel := ""
	if opts.Embedder != nil {
		embedModel = opts.Embedder.Model()
	}

	if entry, ok := memoryCache.Load(indexDir); ok {
		cached := entry.(cacheEntry)
		if cached.fingerprint == fingerprint && cached.model == embedModel {
			return cached.index, nil
		}
	}

	m, chunks, vectors, ok := loadPersisted(indexDir, opts.Embedder != nil)
	if ok && m.SchemaVersion == indexSchemaVersion && m.ChunkerVersion == chunkerVersion &&
		m.Fingerprint == fingerprint && m.FileCount == len(files) &&
		(opts.Embedder == nil || m.EmbedModel == embedModel) {
		idx := &sourceIndex{chunks: chunks, vectors: vectors, bm25: newBM25(chunks), model: m.EmbedModel}
		memoryCache.Store(indexDir, cacheEntry{fingerprint: fingerprint, model: embedModel, index: idx})
		return idx, nil
	}

	idx := buildSource(ctx, files, opts)
	next := manifest{
		SchemaVersion: indexSchemaVersion, ChunkerVersion: chunkerVersion,
		FileCount: len(files), Fingerprint: fingerprint,
	}
	if err := persist(indexDir, next, idx); err != nil {
		slog.Warn("searchindex: persisting the index failed", "error", err, "dir", indexDir)
	}
	memoryCache.Store(indexDir, cacheEntry{fingerprint: fingerprint, model: idx.model, index: idx})
	return idx, nil
}

func scanWorkspace(workspace string) ([]fileEntry, string) {
	entries := make([]fileEntry, 0)
	_ = filepath.WalkDir(workspace, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := dirEntry.Name()
		if name != "." && strings.HasPrefix(name, ".") {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if dirEntry.IsDir() || dirEntry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if _, ok := indexableExtensions[strings.ToLower(filepath.Ext(name))]; !ok {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, loreStorePrefix) {
			return nil
		}
		if _, safeErr := book.SafePath(workspace, rel); safeErr != nil {
			return nil
		}
		info, infoErr := dirEntry.Info()
		if infoErr != nil || info.Size() > maxIndexedFileBytes {
			return nil
		}
		entries = append(entries, fileEntry{
			rel: rel, abs: path, size: info.Size(), modTime: info.ModTime().UnixNano(),
		})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	hash := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", entry.rel, entry.size, entry.modTime)
	}
	return entries, hex.EncodeToString(hash.Sum(nil))
}

func buildSource(ctx context.Context, files []fileEntry, opts Options) *sourceIndex {
	var chunks []chunk
	for _, file := range files {
		data, err := os.ReadFile(file.abs)
		if err != nil {
			continue
		}
		text := strings.ToValidUTF8(string(data), "")
		title := fileTitle(file.rel, text)
		for _, piece := range chunkText(text) {
			chunks = append(chunks, chunk{
				ID: file.rel + "#" + strconv.Itoa(piece.Start),
				Path: file.rel, Title: title, Start: piece.Start, Text: piece.Text,
			})
		}
	}
	idx := &sourceIndex{chunks: chunks}
	if opts.Embedder != nil && len(chunks) > 0 {
		vectors, err := embedChunks(ctx, opts.IndexDir, opts.Embedder, chunks)
		if err != nil {
			slog.Warn("searchindex: embedding failed, serving keyword results", "error", err, "workspace", opts.Workspace)
		} else {
			idx.vectors = vectors
			idx.model = opts.Embedder.Model()
		}
	}
	idx.bm25 = newBM25(chunks)
	return idx
}

func fileTitle(rel, text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		break
	}
	base := filepath.Base(rel)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func embedChunks(ctx context.Context, indexDir string, embedder Embedder, chunks []chunk) ([][]float32, error) {
	cache := loadEmbedCache(indexDir)
	keys := make([]string, len(chunks))
	var missingTexts, missingKeys []string
	for index, item := range chunks {
		keys[index] = embeddingCacheKey(embedder.Model(), item.Text)
		if _, ok := cache[keys[index]]; !ok {
			missingTexts = append(missingTexts, item.Text)
			missingKeys = append(missingKeys, keys[index])
		}
	}
	if len(missingTexts) > 0 {
		embedded, err := embedder.EmbedDocuments(ctx, missingTexts)
		if err != nil {
			return nil, err
		}
		for index, vector := range embedded {
			if len(vector) == 0 {
				// The endpoint rejected this single input (for example one
				// longer than its physical batch); this chunk stays
				// keyword-only instead of failing the whole build.
				continue
			}
			cache[missingKeys[index]] = normalizeVector(vector)
		}
	}
	dim := 0
	vectors := make([][]float32, len(chunks))
	for index := range chunks {
		vector := cache[keys[index]]
		if len(vector) == 0 {
			continue
		}
		if dim == 0 {
			dim = len(vector)
		} else if len(vector) != dim {
			return nil, fmt.Errorf("inconsistent embedding dimension %d and %d", len(vector), dim)
		}
		vectors[index] = vector
	}
	if dim == 0 {
		return nil, fmt.Errorf("the embedding endpoint produced no vectors for %d chunks", len(chunks))
	}
	saveEmbedCache(indexDir, keys, vectors)
	return vectors, nil
}

func embeddingCacheKey(model, text string) string {
	hash := sha256.New()
	hash.Write([]byte(model))
	hash.Write([]byte{0})
	hash.Write([]byte(text))
	return hex.EncodeToString(hash.Sum(nil))
}

func persist(indexDir string, m manifest, idx *sourceIndex) error {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(indexDir, chunksFileName), func(writer io.Writer) error {
		buffered := bufio.NewWriter(writer)
		encoder := json.NewEncoder(buffered)
		for _, item := range idx.chunks {
			if err := encoder.Encode(item); err != nil {
				return err
			}
		}
		return buffered.Flush()
	}); err != nil {
		return err
	}
	vectorsPath := filepath.Join(indexDir, vectorsFileName)
	vectorDim := 0
	for _, vector := range idx.vectors {
		if len(vector) > 0 {
			vectorDim = len(vector)
			break
		}
	}
	if vectorDim > 0 {
		m.EmbedDim = vectorDim
		if err := writeFileAtomic(vectorsPath, func(writer io.Writer) error {
			buffered := bufio.NewWriter(writer)
			for _, vector := range idx.vectors {
				if len(vector) != vectorDim {
					// A chunk the endpoint rejected keeps a zero vector so the
					// dense file stays aligned; zeros never win a cosine ranking.
					vector = make([]float32, vectorDim)
				}
				if err := binary.Write(buffered, binary.LittleEndian, vector); err != nil {
					return err
				}
			}
			return buffered.Flush()
		}); err != nil {
			return err
		}
	} else {
		m.EmbedDim = 0
		_ = os.Remove(vectorsPath)
	}
	m.EmbedModel = idx.model
	return writeFileAtomic(filepath.Join(indexDir, manifestFileName), func(writer io.Writer) error {
		encoded, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = writer.Write(encoded)
		return err
	})
}

func loadPersisted(indexDir string, withVectors bool) (manifest, []chunk, [][]float32, bool) {
	raw, err := os.ReadFile(filepath.Join(indexDir, manifestFileName))
	if err != nil {
		return manifest{}, nil, nil, false
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, nil, nil, false
	}
	chunks, err := readChunks(filepath.Join(indexDir, chunksFileName))
	if err != nil {
		return manifest{}, nil, nil, false
	}
	var vectors [][]float32
	if withVectors && m.EmbedDim > 0 {
		vectors, err = readVectors(filepath.Join(indexDir, vectorsFileName), len(chunks), m.EmbedDim)
		if err != nil {
			return manifest{}, nil, nil, false
		}
	}
	return m, chunks, vectors, true
}

func readChunks(path string) ([]chunk, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var chunks []chunk
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var item chunk
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, err
		}
		chunks = append(chunks, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

func readVectors(path string, count, dim int) ([][]float32, error) {
	if count == 0 {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expected := count * dim * 4
	if len(raw) != expected {
		return nil, fmt.Errorf("vector file size %d, want %d", len(raw), expected)
	}
	vectors := make([][]float32, count)
	for index := 0; index < count; index++ {
		vector := make([]float32, dim)
		base := index * dim * 4
		for i := 0; i < dim; i++ {
			vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[base+i*4:]))
		}
		vectors[index] = vector
	}
	return vectors, nil
}

func loadEmbedCache(indexDir string) map[string][]float32 {
	cache := make(map[string][]float32)
	raw, err := os.ReadFile(filepath.Join(indexDir, cacheFileName))
	if err != nil || len(raw) < 8 || string(raw[:4]) != embedCacheMagic {
		return cache
	}
	dim := int(binary.LittleEndian.Uint32(raw[4:8]))
	if dim <= 0 || dim > 65536 {
		return cache
	}
	recordSize := 32 + dim*4
	body := raw[8:]
	if len(body)%recordSize != 0 {
		return cache
	}
	for offset := 0; offset+recordSize <= len(body); offset += recordSize {
		key := hex.EncodeToString(body[offset : offset+32])
		vector := make([]float32, dim)
		for i := 0; i < dim; i++ {
			vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(body[offset+32+i*4:]))
		}
		cache[key] = vector
	}
	return cache
}

func saveEmbedCache(indexDir string, keys []string, vectors [][]float32) {
	if len(keys) == 0 || len(vectors) == 0 {
		return
	}
	dim := 0
	for _, vector := range vectors {
		if len(vector) > 0 {
			dim = len(vector)
			break
		}
	}
	if dim == 0 {
		return
	}
	var buffer bytes.Buffer
	buffer.WriteString(embedCacheMagic)
	_ = binary.Write(&buffer, binary.LittleEndian, uint32(dim))
	for index, key := range keys {
		raw, err := hex.DecodeString(key)
		if err != nil || len(raw) != 32 || len(vectors[index]) != dim {
			continue
		}
		buffer.Write(raw)
		for _, value := range vectors[index] {
			_ = binary.Write(&buffer, binary.LittleEndian, value)
		}
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		slog.Warn("searchindex: creating the index directory failed", "error", err, "dir", indexDir)
		return
	}
	if err := writeFileAtomic(filepath.Join(indexDir, cacheFileName), func(writer io.Writer) error {
		_, err := writer.Write(buffer.Bytes())
		return err
	}); err != nil {
		slog.Warn("searchindex: saving the embedding cache failed", "error", err, "dir", indexDir)
	}
}

func writeFileAtomic(path string, write func(io.Writer) error) error {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
