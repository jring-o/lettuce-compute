package config

import (
	"strings"
	"testing"
)

// BG-34 regression: LETTUCE_HEAD_REQUIRE_SHARED_STORE=1 with no Redis URL
// configured must be fatal at boot (config validation failure) — a multi-
// replica fleet that silently loses its shared replay/rate-limit store lets a
// replayed signature pass every other replica. With a URL configured the same
// knob validates cleanly.
func TestRequireSharedStoreValidate_BG34(t *testing.T) {
	base := HeadConfig{Name: "test"}

	t.Run("require without redis url fails", func(t *testing.T) {
		h := base
		h.RequireSharedStore = true
		err := h.Validate()
		if err == nil {
			t.Fatal("expected validation error for require_shared_store with empty redis_url, got nil")
		}
		if !strings.Contains(err.Error(), "LETTUCE_REDIS_URL") {
			t.Errorf("error should tell the operator which variable to set, got: %v", err)
		}
	})

	t.Run("require with redis url passes", func(t *testing.T) {
		h := base
		h.RequireSharedStore = true
		h.RedisURL = "redis://:pw@redis:6379"
		if err := h.Validate(); err != nil {
			t.Fatalf("expected valid config, got: %v", err)
		}
	})

	t.Run("no require and no redis url passes (single-replica default)", func(t *testing.T) {
		h := base
		if err := h.Validate(); err != nil {
			t.Fatalf("expected valid config, got: %v", err)
		}
	})
}

func TestRequireSharedStoreEnvOverride_BG34(t *testing.T) {
	path := writeTestConfig(t, minimalConfig)

	t.Setenv("LETTUCE_HEAD_REQUIRE_SHARED_STORE", "1")
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to fail: require_shared_store set with no redis url")
	}

	t.Setenv("LETTUCE_REDIS_URL", "redis://:pw@redis:6379")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected Load to succeed with a redis url, got: %v", err)
	}
	if !cfg.Head.RequireSharedStore {
		t.Error("LETTUCE_HEAD_REQUIRE_SHARED_STORE=1 did not set Head.RequireSharedStore")
	}

	t.Setenv("LETTUCE_HEAD_REQUIRE_SHARED_STORE", "not-a-bool")
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to reject a non-boolean LETTUCE_HEAD_REQUIRE_SHARED_STORE")
	}
}
