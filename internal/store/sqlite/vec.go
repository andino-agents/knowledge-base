package sqlite

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
)

// vecIndex is an in-memory brute-force cosine KNN index over chunk
// embeddings. Vectors persist as little-endian float32 BLOBs in the
// chunk_embeddings table; this index is rebuilt from them on open and kept in
// sync after every committed write.
//
// Brute force is a deliberate choice: at the documented ceiling of ~100k
// chunks x 1024 dims the full scan is a few hundred MB of sequential float
// math, single-digit milliseconds in pure Go, with zero dependencies. An ANN
// structure can replace this behind the same three methods if a deployment
// ever outgrows it.
type vecIndex struct {
	mu    sync.RWMutex
	dim   int
	ids   []int64
	vecs  [][]float32
	norms []float32
	pos   map[int64]int
}

func newVecIndex(dim int) *vecIndex {
	return &vecIndex{dim: dim, pos: map[int64]int{}}
}

func (v *vecIndex) add(id int64, vec []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if i, ok := v.pos[id]; ok {
		v.vecs[i] = vec
		v.norms[i] = norm(vec)
		return
	}
	v.pos[id] = len(v.ids)
	v.ids = append(v.ids, id)
	v.vecs = append(v.vecs, vec)
	v.norms = append(v.norms, norm(vec))
}

func (v *vecIndex) remove(id int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	i, ok := v.pos[id]
	if !ok {
		return
	}
	last := len(v.ids) - 1
	v.ids[i] = v.ids[last]
	v.vecs[i] = v.vecs[last]
	v.norms[i] = v.norms[last]
	v.pos[v.ids[i]] = i
	v.ids = v.ids[:last]
	v.vecs = v.vecs[:last]
	v.norms = v.norms[:last]
	delete(v.pos, id)
}

// search returns the ids of the k nearest vectors by cosine similarity,
// most similar first.
func (v *vecIndex) search(query []float32, k int) []int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	qn := norm(query)
	if qn == 0 || len(v.ids) == 0 {
		return nil
	}
	type scored struct {
		id    int64
		score float32
	}
	results := make([]scored, 0, len(v.ids))
	for i, vec := range v.vecs {
		if v.norms[i] == 0 {
			continue
		}
		results = append(results, scored{v.ids[i], dot(query, vec) / (qn * v.norms[i])})
	}
	sort.Slice(results, func(a, b int) bool {
		if results[a].score != results[b].score {
			return results[a].score > results[b].score
		}
		return results[a].id < results[b].id
	})
	if len(results) > k {
		results = results[:k]
	}
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.id
	}
	return ids
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func norm(a []float32) float32 {
	return float32(math.Sqrt(float64(dot(a, a))))
}

// encodeVec serializes a vector as little-endian float32, the same layout
// sqlite-vec uses, keeping the BLOBs portable.
func encodeVec(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func decodeVec(blob []byte, dim int) ([]float32, error) {
	if len(blob) != 4*dim {
		return nil, fmt.Errorf("embedding blob is %d bytes, want %d for dimension %d", len(blob), 4*dim, dim)
	}
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec, nil
}
