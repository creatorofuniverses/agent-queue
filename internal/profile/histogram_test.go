package profile

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHistogramBuckets(t *testing.T) {
	for _, v := range []uint64{0, 1, 15, 16, 31, 32, 33, 100 << 20, 1 << 40} {
		var h Histogram
		h.Add(v)
		q := h.Quantile(100)
		if q < v || q > v+v/16 || v < 16 && q != v {
			t.Errorf("%d: %d", v, q)
		}
		if v == 100<<20 && q != 109051903 {
			t.Fatal(q)
		}
	}
}
func TestHistogramQuantile(t *testing.T) {
	var h Histogram
	if h.Quantile(90) != 0 {
		t.Fatal(h)
	}
	for i := 0; i < 9; i++ {
		h.Add(100 << 20)
	}
	h.Add(200 << 20)
	if h.Quantile(90) != 109051903 || h.Quantile(100) != 218103807 {
		t.Fatal(h)
	}
}
func TestHistogramMergeAndJSON(t *testing.T) {
	var a, b Histogram
	a.Add(32)
	b.Add(32)
	b.Add(1)
	a.Merge(b)
	if a.Count() != 3 {
		t.Fatal(a)
	}
	j, e := json.Marshal(a)
	if e != nil || string(j) != "[[1,1],[32,2]]" {
		t.Fatal(string(j), e)
	}
	var c Histogram
	if e = json.Unmarshal(j, &c); e != nil || !reflect.DeepEqual(a, c) {
		t.Fatal(c, e)
	}
}
