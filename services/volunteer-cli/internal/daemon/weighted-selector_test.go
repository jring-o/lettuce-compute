package daemon

import (
	"math"
	"testing"
)

func TestSelectHead_SingleServer(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 100})

	srv := &ServerConnection{Name: "alpha", Available: true}
	for i := 0; i < 100; i++ {
		got := ws.SelectHead([]*ServerConnection{srv})
		if got != srv {
			t.Fatalf("iteration %d: expected alpha, got %v", i, got)
		}
		ws.RecordAssignment("alpha", "leaf-1")
	}
}

func TestSelectHead_EqualWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 100, "beta": 100})

	srvA := &ServerConnection{Name: "alpha", Available: true}
	srvB := &ServerConnection{Name: "beta", Available: true}
	available := []*ServerConnection{srvA, srvB}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		head := ws.SelectHead(available)
		counts[head.Name]++
		ws.RecordAssignment(head.Name, "leaf-1")
	}

	// With equal weights, expect ~50/50. Chi-squared test.
	expected := float64(n) / 2.0
	chiSq := math.Pow(float64(counts["alpha"])-expected, 2)/expected +
		math.Pow(float64(counts["beta"])-expected, 2)/expected

	// Chi-squared critical value for 1 df, p < 0.01 is 6.635.
	if chiSq > 6.635 {
		t.Errorf("distribution too skewed: alpha=%d, beta=%d, chi²=%.2f (> 6.635)",
			counts["alpha"], counts["beta"], chiSq)
	}
}

func TestSelectHead_UnequalWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetHeadWeights(map[string]int{"alpha": 300, "beta": 100})

	srvA := &ServerConnection{Name: "alpha", Available: true}
	srvB := &ServerConnection{Name: "beta", Available: true}
	available := []*ServerConnection{srvA, srvB}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		head := ws.SelectHead(available)
		counts[head.Name]++
		ws.RecordAssignment(head.Name, "leaf-1")
	}

	// Expected: alpha=750, beta=250. Chi-squared test.
	expectedA := float64(n) * 0.75
	expectedB := float64(n) * 0.25
	chiSq := math.Pow(float64(counts["alpha"])-expectedA, 2)/expectedA +
		math.Pow(float64(counts["beta"])-expectedB, 2)/expectedB

	if chiSq > 6.635 {
		t.Errorf("distribution too skewed: alpha=%d (want ~750), beta=%d (want ~250), chi²=%.2f",
			counts["alpha"], counts["beta"], chiSq)
	}
}

func TestSelectLeaf_EqualWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetLeafWeights("srv", map[string]int{"a": 100, "b": 100, "c": 100})

	leafs := []CachedLeafInfo{
		{ID: "id-a", Slug: "a"},
		{ID: "id-b", Slug: "b"},
		{ID: "id-c", Slug: "c"},
	}

	counts := map[string]int{}
	n := 900
	for i := 0; i < n; i++ {
		id := ws.SelectLeaf("srv", leafs)
		counts[id]++
		slug := ""
		for _, l := range leafs {
			if l.ID == id {
				slug = l.Slug
			}
		}
		ws.RecordAssignment("srv", slug)
	}

	// Each should be ~300. Chi-squared test with 2 df, p<0.01 critical = 9.210.
	expected := float64(n) / 3.0
	chiSq := 0.0
	for _, id := range []string{"id-a", "id-b", "id-c"} {
		chiSq += math.Pow(float64(counts[id])-expected, 2) / expected
	}
	if chiSq > 9.210 {
		t.Errorf("leaf distribution too skewed: %v, chi²=%.2f", counts, chiSq)
	}
}

func TestSelectLeaf_UnequalWeights(t *testing.T) {
	ws := NewWeightedSelector()
	ws.SetLeafWeights("srv", map[string]int{"a": 50, "b": 30, "c": 20})

	leafs := []CachedLeafInfo{
		{ID: "id-a", Slug: "a"},
		{ID: "id-b", Slug: "b"},
		{ID: "id-c", Slug: "c"},
	}

	counts := map[string]int{}
	n := 1000
	for i := 0; i < n; i++ {
		id := ws.SelectLeaf("srv", leafs)
		counts[id]++
		slug := ""
		for _, l := range leafs {
			if l.ID == id {
				slug = l.Slug
			}
		}
		ws.RecordAssignment("srv", slug)
	}

	// Expected: a=500, b=300, c=200.
	expectedA := float64(n) * 0.5
	expectedB := float64(n) * 0.3
	expectedC := float64(n) * 0.2
	chiSq := math.Pow(float64(counts["id-a"])-expectedA, 2)/expectedA +
		math.Pow(float64(counts["id-b"])-expectedB, 2)/expectedB +
		math.Pow(float64(counts["id-c"])-expectedC, 2)/expectedC

	if chiSq > 9.210 {
		t.Errorf("leaf distribution too skewed: a=%d (want ~500), b=%d (want ~300), c=%d (want ~200), chi²=%.2f",
			counts["id-a"], counts["id-b"], counts["id-c"], chiSq)
	}
}

func TestSelectHead_AllInBackoff(t *testing.T) {
	ws := NewWeightedSelector()
	got := ws.SelectHead(nil)
	if got != nil {
		t.Errorf("expected nil for empty available list, got %v", got)
	}

	got = ws.SelectHead([]*ServerConnection{})
	if got != nil {
		t.Errorf("expected nil for empty available list, got %v", got)
	}
}

func TestRecordAssignment_UpdatesCounts(t *testing.T) {
	ws := NewWeightedSelector()
	ws.RecordAssignment("srv-a", "leaf-1")
	ws.RecordAssignment("srv-a", "leaf-1")
	ws.RecordAssignment("srv-a", "leaf-2")

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.headCounts["srv-a"] != 3 {
		t.Errorf("head count = %d, want 3", ws.headCounts["srv-a"])
	}
	if ws.leafCounts["srv-a"]["leaf-1"] != 2 {
		t.Errorf("leaf-1 count = %d, want 2", ws.leafCounts["srv-a"]["leaf-1"])
	}
	if ws.leafCounts["srv-a"]["leaf-2"] != 1 {
		t.Errorf("leaf-2 count = %d, want 1", ws.leafCounts["srv-a"]["leaf-2"])
	}
}

func TestReset_ClearsCounts(t *testing.T) {
	ws := NewWeightedSelector()
	ws.RecordAssignment("srv-a", "leaf-1")
	ws.RecordAssignment("srv-b", "leaf-2")

	ws.Reset()

	ws.mu.Lock()
	defer ws.mu.Unlock()

	if len(ws.headCounts) != 0 {
		t.Errorf("head counts not cleared: %v", ws.headCounts)
	}
	if len(ws.leafCounts) != 0 {
		t.Errorf("leaf counts not cleared: %v", ws.leafCounts)
	}
}
