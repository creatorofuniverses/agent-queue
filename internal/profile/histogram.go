package profile

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"sort"
)

// Histogram uses sixteen linear sub-buckets per power of two.
type Histogram struct{ buckets map[int]uint64 }

func (h *Histogram) Add(v uint64) {
	idx := int(v)
	if v >= 16 {
		e := bits.Len64(v) - 1
		idx = (e-3)*16 + int((v>>uint(e-4))&15)
	}
	if h.buckets == nil {
		h.buckets = map[int]uint64{}
	}
	h.buckets[idx]++
}
func (h *Histogram) Merge(o Histogram) {
	if h.buckets == nil {
		h.buckets = map[int]uint64{}
	}
	for k, v := range o.buckets {
		h.buckets[k] += v
	}
}
func (h Histogram) Count() (n uint64) {
	for _, v := range h.buckets {
		n += v
	}
	return
}
func (h Histogram) keys() []int {
	ks := make([]int, 0, len(h.buckets))
	for k := range h.buckets {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}
func (h Histogram) Quantile(p int) uint64 {
	n := h.Count()
	if n == 0 {
		return 0
	}
	p = max(1, min(100, p))
	rank := n/100*uint64(p) + (n%100*uint64(p)+99)/100
	var c uint64
	for _, k := range h.keys() {
		c += h.buckets[k]
		if c >= rank {
			if k < 16 {
				return uint64(k)
			}
			shift := uint(k/16 - 1)
			return (uint64(17+k%16) << shift) - 1
		}
	}
	return 0
}
func (h Histogram) MarshalJSON() ([]byte, error) {
	pairs := make([][2]uint64, 0, len(h.buckets))
	for _, k := range h.keys() {
		pairs = append(pairs, [2]uint64{uint64(k), h.buckets[k]})
	}
	return json.Marshal(pairs)
}
func (h *Histogram) UnmarshalJSON(b []byte) error {
	var pairs [][2]uint64
	if e := json.Unmarshal(b, &pairs); e != nil {
		return e
	}
	m := map[int]uint64{}
	for _, p := range pairs {
		if p[0] > 975 {
			return fmt.Errorf("histogram bucket out of range")
		}
		if p[1] > 0 {
			m[int(p[0])] += p[1]
		}
	}
	h.buckets = m
	return nil
}
