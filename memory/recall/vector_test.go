package recall

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"parallel-scaled", []float32{1, 0, 0}, []float32{5, 0, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"zero-vector", []float32{0, 0}, []float32{1, 1}, 0},
		{"both-zero", []float32{0, 0}, []float32{0, 0}, 0},
		{"dim-mismatch", []float32{1, 2}, []float32{1, 2, 3}, 0},
		{"empty", []float32{}, []float32{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Cosine(c.a, c.b)
			if math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("Cosine(%v,%v)=%v want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// 半相关：45° 夹角应得 cos=√2/2，验证不是只会判 0/1。
func TestCosine_PartialAngle(t *testing.T) {
	got := Cosine([]float32{1, 0}, []float32{1, 1})
	want := math.Sqrt2 / 2
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Cosine=%v want %v", got, want)
	}
}
