package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// --- initializeWeights tests ---

func newWeightTestDaemon(servers []config.ServerConfig) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false
	cfg.Servers = servers

	d := NewDaemon(DaemonConfig{
		Config:  cfg,
		PubKey:  pub,
		PrivKey: priv,
		Logger:  logger,
	})
	return d
}

func TestInitializeWeights_ALLMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			Weight:      200,
			LeafPreferences: config.LeafPreferences{
				Mode:    "ALL",
				Weights: map[string]int{"leaf-x": 500},
			},
		},
	})

	// Populate leaf cache with default weights.
	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name: "srv-a",
		Leafs: []CachedLeafInfo{
			{Slug: "leaf-x"},
			{Slug: "leaf-y"},
		},
		DefaultWeights: map[string]int{
			"leaf-x": 100,
			"leaf-y": 200,
		},
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	// Head weight should be 200.
	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	if d.weightedSelector.headWeights["srv-a"] != 200 {
		t.Errorf("head weight = %d, want 200", d.weightedSelector.headWeights["srv-a"])
	}

	// ALL mode: researcher defaults + custom overrides.
	lw := d.weightedSelector.leafWeights["srv-a"]
	if lw == nil {
		t.Fatal("expected leaf weights for srv-a")
	}
	// leaf-x: custom override 500 (not researcher default 100).
	if lw["leaf-x"] != 500 {
		t.Errorf("leaf-x weight = %d, want 500 (custom override)", lw["leaf-x"])
	}
	// leaf-y: researcher default 200 (no custom override).
	if lw["leaf-y"] != 200 {
		t.Errorf("leaf-y weight = %d, want 200 (researcher default)", lw["leaf-y"])
	}
}

func TestInitializeWeights_SPECIFICMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:    "SPECIFIC",
				Enabled: []string{"leaf-x", "leaf-z"},
				Weights: map[string]int{"leaf-z": 300},
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name: "srv-a",
		Leafs: []CachedLeafInfo{
			{Slug: "leaf-x"},
			{Slug: "leaf-y"},
			{Slug: "leaf-z"},
		},
		DefaultWeights: map[string]int{
			"leaf-x": 150,
			"leaf-y": 200,
			"leaf-z": 50,
		},
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	lw := d.weightedSelector.leafWeights["srv-a"]
	if lw == nil {
		t.Fatal("expected leaf weights for srv-a")
	}

	// leaf-x: enabled, has researcher default 150, no custom override.
	if lw["leaf-x"] != 150 {
		t.Errorf("leaf-x weight = %d, want 150", lw["leaf-x"])
	}
	// leaf-y: NOT enabled â€” should not appear.
	if _, ok := lw["leaf-y"]; ok {
		t.Errorf("leaf-y should not be in weights (not in enabled list)")
	}
	// leaf-z: enabled, custom override 300.
	if lw["leaf-z"] != 300 {
		t.Errorf("leaf-z weight = %d, want 300", lw["leaf-z"])
	}
}

func TestInitializeWeights_SPECIFICMode_NoDefault(t *testing.T) {
	// A leaf that is enabled but has no researcher default should get weight 100.
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:    "SPECIFIC",
				Enabled: []string{"leaf-new"},
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name:           "srv-a",
		Leafs:          []CachedLeafInfo{{Slug: "leaf-new"}},
		DefaultWeights: map[string]int{}, // no defaults
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	lw := d.weightedSelector.leafWeights["srv-a"]
	if lw["leaf-new"] != 100 {
		t.Errorf("leaf-new weight = %d, want 100 (fallback default)", lw["leaf-new"])
	}
}

func TestInitializeWeights_BLOCKLISTMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:     "BLOCKLIST",
				Disabled: []string{"leaf-y"},
				Weights:  map[string]int{"leaf-x": 400},
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name: "srv-a",
		Leafs: []CachedLeafInfo{
			{Slug: "leaf-x"},
			{Slug: "leaf-y"},
			{Slug: "leaf-z"},
		},
		DefaultWeights: map[string]int{
			"leaf-x": 100,
			"leaf-y": 200,
		},
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	lw := d.weightedSelector.leafWeights["srv-a"]
	if lw == nil {
		t.Fatal("expected leaf weights for srv-a")
	}

	// leaf-x: not blocked, custom override 400.
	if lw["leaf-x"] != 400 {
		t.Errorf("leaf-x weight = %d, want 400", lw["leaf-x"])
	}
	// leaf-y: blocked â€” should not appear.
	if _, ok := lw["leaf-y"]; ok {
		t.Errorf("leaf-y should not be in weights (blocked)")
	}
	// leaf-z: not in defaults but in cache, not blocked â†’ gets default 100.
	if lw["leaf-z"] != 100 {
		t.Errorf("leaf-z weight = %d, want 100 (fallback for cache-only leaf)", lw["leaf-z"])
	}
}

func TestInitializeWeights_DefaultHeadWeight(t *testing.T) {
	// Weight=0 in config should default to 100.
	d := newWeightTestDaemon([]config.ServerConfig{
		{GRPCAddress: "localhost:9090", Name: "srv-a", Weight: 0},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name:           "srv-a",
		DefaultWeights: map[string]int{},
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	if d.weightedSelector.headWeights["srv-a"] != 100 {
		t.Errorf("head weight = %d, want 100 (default)", d.weightedSelector.headWeights["srv-a"])
	}
}

func TestInitializeWeights_EmptyModeDefaultsToALL(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress:     "localhost:9090",
			Name:            "srv-a",
			LeafPreferences: config.LeafPreferences{Mode: ""},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Name:           "srv-a",
		Leafs:          []CachedLeafInfo{{Slug: "leaf-a"}},
		DefaultWeights: map[string]int{"leaf-a": 150},
	}
	d.leafCache.mu.Unlock()

	d.initializeWeights()

	d.weightedSelector.mu.Lock()
	defer d.weightedSelector.mu.Unlock()

	lw := d.weightedSelector.leafWeights["srv-a"]
	if lw["leaf-a"] != 150 {
		t.Errorf("leaf-a weight = %d, want 150 (ALL mode default)", lw["leaf-a"])
	}
}

// --- enabledLeafs tests ---

func TestEnabledLeafs_ALLMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{GRPCAddress: "localhost:9090", Name: "srv-a"},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Leafs: []CachedLeafInfo{
			{Slug: "a"}, {Slug: "b"}, {Slug: "c"},
		},
	}
	d.leafCache.mu.Unlock()

	enabled := d.enabledLeafs("srv-a")
	if len(enabled) != 3 {
		t.Errorf("enabled count = %d, want 3", len(enabled))
	}
}

func TestEnabledLeafs_SPECIFICMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:    "SPECIFIC",
				Enabled: []string{"a", "c"},
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Leafs: []CachedLeafInfo{
			{Slug: "a"}, {Slug: "b"}, {Slug: "c"},
		},
	}
	d.leafCache.mu.Unlock()

	enabled := d.enabledLeafs("srv-a")
	if len(enabled) != 2 {
		t.Fatalf("enabled count = %d, want 2", len(enabled))
	}
	slugs := map[string]bool{}
	for _, l := range enabled {
		slugs[l.Slug] = true
	}
	if !slugs["a"] || !slugs["c"] {
		t.Errorf("expected a and c, got %v", slugs)
	}
	if slugs["b"] {
		t.Errorf("b should not be enabled in SPECIFIC mode")
	}
}

func TestEnabledLeafs_BLOCKLISTMode(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode:     "BLOCKLIST",
				Disabled: []string{"b"},
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Leafs: []CachedLeafInfo{
			{Slug: "a"}, {Slug: "b"}, {Slug: "c"},
		},
	}
	d.leafCache.mu.Unlock()

	enabled := d.enabledLeafs("srv-a")
	if len(enabled) != 2 {
		t.Fatalf("enabled count = %d, want 2", len(enabled))
	}
	slugs := map[string]bool{}
	for _, l := range enabled {
		slugs[l.Slug] = true
	}
	if slugs["b"] {
		t.Errorf("b should be blocked")
	}
	if !slugs["a"] || !slugs["c"] {
		t.Errorf("a and c should be enabled, got %v", slugs)
	}
}

func TestEnabledLeafs_NilCache(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{GRPCAddress: "localhost:9090", Name: "srv-a"},
	})
	// Do not populate cache.
	enabled := d.enabledLeafs("srv-a")
	if enabled != nil {
		t.Errorf("expected nil for uncached server, got %v", enabled)
	}
}

func TestEnabledLeafs_UnknownModeFallsBackToAll(t *testing.T) {
	d := newWeightTestDaemon([]config.ServerConfig{
		{
			GRPCAddress: "localhost:9090",
			Name:        "srv-a",
			LeafPreferences: config.LeafPreferences{
				Mode: "UNKNOWN",
			},
		},
	})

	d.leafCache.mu.Lock()
	d.leafCache.heads["srv-a"] = &CachedHeadInfo{
		Leafs: []CachedLeafInfo{{Slug: "a"}, {Slug: "b"}},
	}
	d.leafCache.mu.Unlock()

	enabled := d.enabledLeafs("srv-a")
	if len(enabled) != 2 {
		t.Errorf("unknown mode should return all leafs, got %d", len(enabled))
	}
}

// --- availableServers tests ---

func TestAvailableServers_AllAvailable(t *testing.T) {
	d := newWeightTestDaemon(nil)
	srvA := &ServerConnection{Name: "a", Available: true}
	srvB := &ServerConnection{Name: "b", Available: true}
	d.multiClient = NewMultiServerClient([]*ServerConnection{srvA, srvB}, d.logger)

	available := d.availableServers()
	if len(available) != 2 {
		t.Errorf("available count = %d, want 2", len(available))
	}
}

func TestAvailableServers_OneInBackoff(t *testing.T) {
	d := newWeightTestDaemon(nil)
	srvA := &ServerConnection{Name: "a", Available: true}
	srvB := &ServerConnection{
		Name:      "b",
		Available: false,
		LastError: time.Now(),
		Backoff:   1 * time.Hour, // far future
	}
	d.multiClient = NewMultiServerClient([]*ServerConnection{srvA, srvB}, d.logger)

	available := d.availableServers()
	if len(available) != 1 {
		t.Fatalf("available count = %d, want 1", len(available))
	}
	if available[0].Name != "a" {
		t.Errorf("expected server a, got %s", available[0].Name)
	}
}

func TestAvailableServers_BackoffExpired(t *testing.T) {
	d := newWeightTestDaemon(nil)
	srvA := &ServerConnection{
		Name:      "a",
		Available: false,
		LastError: time.Now().Add(-2 * time.Hour), // 2 hours ago
		Backoff:   1 * time.Hour,                  // 1 hour backoff â€” expired
	}
	d.multiClient = NewMultiServerClient([]*ServerConnection{srvA}, d.logger)

	available := d.availableServers()
	if len(available) != 1 {
		t.Errorf("expected server a (backoff expired), got count %d", len(available))
	}
}

func TestAvailableServers_NoneAvailable(t *testing.T) {
	d := newWeightTestDaemon(nil)
	srvA := &ServerConnection{
		Name:      "a",
		Available: false,
		LastError: time.Now(),
		Backoff:   1 * time.Hour,
	}
	d.multiClient = NewMultiServerClient([]*ServerConnection{srvA}, d.logger)

	available := d.availableServers()
	if len(available) != 0 {
		t.Errorf("available count = %d, want 0", len(available))
	}
}

// --- filterOut tests ---

func TestFilterOut(t *testing.T) {
	servers := []*ServerConnection{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	}
	excluded := map[string]bool{"b": true}

	result := filterOut(servers, excluded)
	if len(result) != 2 {
		t.Fatalf("result count = %d, want 2", len(result))
	}
	names := map[string]bool{}
	for _, s := range result {
		names[s.Name] = true
	}
	if names["b"] {
		t.Error("b should be filtered out")
	}
}

func TestFilterOut_Empty(t *testing.T) {
	result := filterOut(nil, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

// --- SelectLeaf/SelectHead edge cases ---

func TestSelectLeaf_EmptyList(t *testing.T) {
	ws := NewWeightedSelector()
	id := ws.SelectLeaf("srv", nil)
	if id != "" {
		t.Errorf("expected empty string for empty leaf list, got %q", id)
	}

	id = ws.SelectLeaf("srv", []CachedLeafInfo{})
	if id != "" {
		t.Errorf("expected empty string for empty leaf list, got %q", id)
	}
}

func TestSelectHead_DefaultWeight(t *testing.T) {
	// When a server has no weight in the map, it defaults to 100.
	ws := NewWeightedSelector()
	// Do NOT call SetHeadWeights â€” weights map is empty.

	srvA := &ServerConnection{Name: "alpha", Available: true}
	srvB := &ServerConnection{Name: "beta", Available: true}

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		head := ws.SelectHead([]*ServerConnection{srvA, srvB})
		if head == nil {
			t.Fatal("expected non-nil head")
		}
		counts[head.Name]++
		ws.RecordAssignment(head.Name, "leaf")
	}

	// Both should get roughly 50 (both default to 100 weight).
	if counts["alpha"] < 40 || counts["beta"] < 40 {
		t.Errorf("default weights should produce roughly equal distribution: alpha=%d, beta=%d",
			counts["alpha"], counts["beta"])
	}
}
