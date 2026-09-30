package daemon

import (
	"strings"
	"testing"
)

// On a 256-thread machine the preview starts 151 tasks together, and one leaf on
// its own runs 66 tasks of two widths. `doctor` says that in a line each: one entry
// per leaf and core count, and each figure once with how many tasks get it — not
// 151 entries and 66 numbers.
func TestDescribeRunPreview_LargeMachineIsGrouped(t *testing.T) {
	var p RunPreview
	add := func(n int, id, name string, cores int) {
		for i := 0; i < n; i++ {
			p.Together = append(p.Together, PreviewTask{LeafID: id, LeafName: name, Head: "h", Cores: cores})
		}
	}
	add(30, "native", "Beyblade Arena (native)", 1)
	add(30, "f13", "GREP f13 (CPU)", 2)
	add(30, "native", "Beyblade Arena (native)", 1)
	add(30, "f14", "GREP f14 (CPU)", 2)
	add(15, "v1", "GREP V1 (CPU)", 4)
	add(16, "box", "Beyblade Arena", 1)
	p.CPULimit, p.TogetherCores = 256, 256
	v1 := make([]int, 0, 66)
	for i := 0; i < 62; i++ {
		v1 = append(v1, 4)
	}
	v1 = append(v1, 2, 2, 2, 2)
	p.Alone = []PreviewLeaf{
		{LeafID: "v1", LeafName: "GREP V1 (CPU)", Head: "h", Tasks: v1, CoresUsed: 256},
		{LeafID: "f13", LeafName: "GREP f13 (CPU)", Head: "h", Tasks: []int{2, 2}, CoresUsed: 4},
		{LeafID: "gpu", LeafName: "GREP V1 (GPU)", Head: "h", CannotStart: "needs a GPU"},
	}

	together, alone := DescribeRunPreview(p)
	if want := "Beyblade Arena (native) × 60 · 1 core each, GREP f13 (CPU) × 30 · 2 cores each, GREP f14 (CPU) × 30 · 2 cores each, " +
		"GREP V1 (CPU) × 15 · 4 cores each, Beyblade Arena × 16 · 1 core each — 256 of 256 cores"; together != want {
		t.Errorf("together =\n  %q\nwant\n  %q", together, want)
	}
	if want := "GREP V1 (CPU) 66 at once: 62 × 4 cores, 4 × 2 cores; GREP f13 (CPU) 2 at once, 2 cores each; GREP V1 (GPU) cannot start here"; alone != want {
		t.Errorf("alone =\n  %q\nwant\n  %q", alone, want)
	}
	if n := strings.Count(together, "Beyblade Arena (native)"); n != 1 {
		t.Errorf("the native leaf is named %d times, want once", n)
	}
}
