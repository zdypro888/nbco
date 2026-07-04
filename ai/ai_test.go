package ai

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(float64(got)-1) > 1e-6 {
		t.Errorf("同向量应为 1, got %v", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(float64(got)) > 1e-6 {
		t.Errorf("正交应为 0, got %v", got)
	}
	if got := Cosine([]float32{1, 1}, []float32{2, 2}); math.Abs(float64(got)-1) > 1e-6 {
		t.Errorf("同方向应为 1, got %v", got)
	}
	// 维度不一致、零向量、空 → 0，不 panic。
	if Cosine([]float32{1}, []float32{1, 2}) != 0 || Cosine([]float32{0, 0}, []float32{1, 1}) != 0 || Cosine(nil, nil) != 0 {
		t.Error("异常输入应返回 0")
	}
	// 相似度排序：a 与 b 更近于 a 与 c。
	a, b, c := []float32{1, 1, 0}, []float32{1, 0.9, 0}, []float32{0, 0, 1}
	if Cosine(a, b) <= Cosine(a, c) {
		t.Error("相近向量的余弦应更大")
	}
}
