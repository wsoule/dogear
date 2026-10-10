// Package embed computes Voyage AI text embeddings and keeps an in-memory
// index of item vectors for semantic search. Vectors are 256-dimension int8
// (256 bytes per item), so ~100k items fit in about 25 MB and a full scan
// takes a few milliseconds, without needing pgvector.
package embed

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	Model = "voyage-4-lite"
	Dims  = 256
)

// Client calls the Voyage embeddings API.
type Client struct {
	APIKey string
	HTTP   *http.Client
	URL    string // default https://api.voyageai.com/v1/embeddings
}

func (c *Client) Enabled() bool { return c != nil && c.APIKey != "" }

// Embed returns one int8 vector per input. kind is "document" for items and
// "query" for searches (Voyage tunes the two differently).
func (c *Client) Embed(ctx context.Context, inputs []string, kind string) ([][]int8, error) {
	body, _ := json.Marshal(map[string]any{"input": inputs, "model": Model, "input_type": kind,
		"output_dimension": Dims, "output_dtype": "int8", "truncation": true})
	u := c.URL
	if u == "" {
		u = "https://api.voyageai.com/v1/embeddings"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Data []struct {
			Embedding []int `json:"embedding"`
			Index     int   `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("voyage decode: %w", err)
	}
	vecs := make([][]int8, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) || len(d.Embedding) != Dims {
			return nil, fmt.Errorf("voyage: unexpected embedding shape")
		}
		v := make([]int8, Dims)
		for i, x := range d.Embedding {
			v[i] = int8(max(-128, min(127, x)))
		}
		vecs[d.Index] = v
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("voyage: missing embedding %d", i)
		}
	}
	return vecs, nil
}

// Bytes and FromBytes convert vectors for storage in a BYTEA column.
func Bytes(v []int8) []byte {
	b := make([]byte, len(v))
	for i, x := range v {
		b[i] = byte(x)
	}
	return b
}

func FromBytes(b []byte) []int8 {
	v := make([]int8, len(b))
	for i, x := range b {
		v[i] = int8(x)
	}
	return v
}

// Index is an in-memory set of item vectors, normalized for cosine
// similarity at insert time.
type Index struct {
	mu    sync.RWMutex
	ids   []int64
	vecs  [][]int8
	norms []float32
	pos   map[int64]int
}

func NewIndex() *Index { return &Index{pos: map[int64]int{}} }

func norm(v []int8) float32 {
	var s int64
	for _, x := range v {
		s += int64(x) * int64(x)
	}
	return float32(math.Sqrt(float64(s)))
}

// Put adds or replaces an item's vector.
func (ix *Index) Put(id int64, v []int8) {
	if len(v) != Dims {
		return
	}
	n := norm(v)
	if n == 0 {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if p, ok := ix.pos[id]; ok {
		ix.vecs[p], ix.norms[p] = v, n
		return
	}
	ix.pos[id] = len(ix.ids)
	ix.ids = append(ix.ids, id)
	ix.vecs = append(ix.vecs, v)
	ix.norms = append(ix.norms, n)
}

// Get returns an item's vector, if indexed.
func (ix *Index) Get(id int64) ([]int8, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.pos[id]
	if !ok {
		return nil, false
	}
	return ix.vecs[p], true
}

func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.ids)
}

type hit struct {
	id    int64
	score float32
}

type minHeap []hit

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(hit)) }
func (h *minHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Match is one search result.
type Match struct {
	ID    int64
	Score float32 // cosine similarity, -1..1
}

// Nearest returns the k items most similar to q, skipping exclude.
func (ix *Index) Nearest(q []int8, k int, exclude int64) []Match {
	qn := norm(q)
	if qn == 0 || k <= 0 {
		return nil
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	h := &minHeap{}
	for i, v := range ix.vecs {
		if ix.ids[i] == exclude {
			continue
		}
		var dot int32
		for j := range Dims {
			dot += int32(q[j]) * int32(v[j])
		}
		s := float32(dot) / (qn * ix.norms[i])
		if h.Len() < k {
			heap.Push(h, hit{ix.ids[i], s})
		} else if s > (*h)[0].score {
			(*h)[0] = hit{ix.ids[i], s}
			heap.Fix(h, 0)
		}
	}
	out := make([]Match, h.Len())
	for i := range out {
		x := (*h)[i]
		out[i] = Match{ID: x.id, Score: x.score}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
