package searchindex

import (
	"math"
	"sort"
)

// scored is one document in a single ranking channel.
type scored struct {
	doc   int
	score float64
}

type posting struct {
	doc int
	tf  float64
}

// bm25Index is a complete in-memory BM25 index over chunk texts. Corpora are a
// single book workspace (thousands of chunks), so a plain inverted index with
// map iteration is sufficient and avoids a heavyweight search dependency.
type bm25Index struct {
	postings map[string][]posting
	docLen   []float64
	avgLen   float64
}

func newBM25(chunks []chunk) *bm25Index {
	postings := make(map[string][]posting)
	docLen := make([]float64, len(chunks))
	total := 0.0
	for index, item := range chunks {
		counts := make(map[string]int)
		for _, token := range tokenize(item.Text) {
			counts[token]++
		}
		docLen[index] = float64(len(counts))
		total += docLen[index]
		for token, tf := range counts {
			postings[token] = append(postings[token], posting{doc: index, tf: float64(tf)})
		}
	}
	avgLen := 0.0
	if len(chunks) > 0 {
		avgLen = total / float64(len(chunks))
	}
	return &bm25Index{postings: postings, docLen: docLen, avgLen: avgLen}
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

func (b *bm25Index) search(query string, limit int) []scored {
	tokens := tokenize(query)
	if len(tokens) == 0 || len(b.docLen) == 0 {
		return nil
	}
	scores := make(map[int]float64)
	documentCount := float64(len(b.docLen))
	for _, token := range tokens {
		list := b.postings[token]
		if len(list) == 0 {
			continue
		}
		df := float64(len(list))
		idf := math.Log(1 + (documentCount-df+0.5)/(df+0.5))
		for _, entry := range list {
			norm := 1.0
			if b.avgLen > 0 {
				norm = 1 - bm25B + bm25B*b.docLen[entry.doc]/b.avgLen
			}
			scores[entry.doc] += idf * entry.tf * (bm25K1 + 1) / (entry.tf + bm25K1*norm)
		}
	}
	return topScored(scores, limit)
}

// searchVectors ranks chunks by cosine similarity. Both the stored vectors and
// the query vector are L2-normalized, so the dot product is the cosine.
func searchVectors(vectors [][]float32, query []float32, limit int) []scored {
	scores := make(map[int]float64, len(vectors))
	for index, vector := range vectors {
		if len(vector) != len(query) {
			continue
		}
		dot := 0.0
		for i, value := range vector {
			dot += float64(value) * float64(query[i])
		}
		if dot > 0 {
			scores[index] = dot
		}
	}
	return topScored(scores, limit)
}

// fuse merges ranking channels with reciprocal rank fusion (k=60).
func fuse(lists [][]scored, limit int) []scored {
	const k = 60.0
	scores := make(map[int]float64)
	for _, list := range lists {
		for rank, item := range list {
			scores[item.doc] += 1 / (k + float64(rank) + 1)
		}
	}
	return topScored(scores, limit)
}

func topScored(scores map[int]float64, limit int) []scored {
	if len(scores) == 0 {
		return nil
	}
	ranked := make([]scored, 0, len(scores))
	for doc, score := range scores {
		ranked = append(ranked, scored{doc: doc, score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].doc < ranked[j].doc
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

func normalizeVector(vector []float32) []float32 {
	norm := 0.0
	for _, value := range vector {
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return vector
	}
	scale := float32(1 / math.Sqrt(norm))
	normalized := make([]float32, len(vector))
	for index, value := range vector {
		normalized[index] = value * scale
	}
	return normalized
}
