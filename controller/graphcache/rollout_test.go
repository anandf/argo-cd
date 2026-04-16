package graphcache

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRolloutConfig_AllStrategy(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:  RolloutStrategyAll,
		Allowlist: make(map[string]bool),
	}

	assert.True(t, rc.ShouldUseGraphCache("https://cluster1.example.com"))
	assert.True(t, rc.ShouldUseGraphCache("https://cluster2.example.com"))
	assert.True(t, rc.ShouldUseGraphCache("https://kubernetes.default.svc"))
}

func TestRolloutConfig_AllowlistStrategy(t *testing.T) {
	rc := &RolloutConfig{
		Strategy: RolloutStrategyAllowlist,
		Allowlist: map[string]bool{
			"https://cluster1.example.com":    true,
			"https://kubernetes.default.svc":  true,
		},
	}

	assert.True(t, rc.ShouldUseGraphCache("https://cluster1.example.com"))
	assert.True(t, rc.ShouldUseGraphCache("https://kubernetes.default.svc"))
	assert.False(t, rc.ShouldUseGraphCache("https://cluster2.example.com"))
	assert.False(t, rc.ShouldUseGraphCache("https://unknown.example.com"))
}

func TestRolloutConfig_PercentageStrategy_100(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:   RolloutStrategyPercentage,
		Percentage: 100,
		Allowlist:  make(map[string]bool),
	}

	// All clusters should be included at 100%
	for i := 0; i < 100; i++ {
		assert.True(t, rc.ShouldUseGraphCache(fmt.Sprintf("https://cluster%d.example.com", i)))
	}
}

func TestRolloutConfig_PercentageStrategy_0(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:   RolloutStrategyPercentage,
		Percentage: 0,
		Allowlist:  make(map[string]bool),
	}

	// No clusters should be included at 0%
	for i := 0; i < 100; i++ {
		assert.False(t, rc.ShouldUseGraphCache(fmt.Sprintf("https://cluster%d.example.com", i)))
	}
}

func TestRolloutConfig_PercentageStrategy_Deterministic(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:   RolloutStrategyPercentage,
		Percentage: 50,
		Allowlist:  make(map[string]bool),
	}

	server := "https://cluster1.example.com"
	firstResult := rc.ShouldUseGraphCache(server)

	// Same input should always produce same output
	for i := 0; i < 100; i++ {
		assert.Equal(t, firstResult, rc.ShouldUseGraphCache(server))
	}
}

func TestRolloutConfig_PercentageStrategy_Distribution(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:   RolloutStrategyPercentage,
		Percentage: 50,
		Allowlist:  make(map[string]bool),
	}

	enabled := 0
	total := 1000
	for i := 0; i < total; i++ {
		if rc.ShouldUseGraphCache(fmt.Sprintf("https://cluster%d.example.com", i)) {
			enabled++
		}
	}

	// Should be roughly 50% (within 10% margin)
	ratio := float64(enabled) / float64(total)
	assert.InDelta(t, 0.5, ratio, 0.1, "Expected roughly 50%% of clusters to be enabled, got %.2f%%", ratio*100)
}

func TestRolloutConfig_UpdateStrategy(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:  RolloutStrategyAllowlist,
		Allowlist: make(map[string]bool),
	}

	assert.False(t, rc.ShouldUseGraphCache("https://cluster1.example.com"))

	rc.UpdateStrategy(RolloutStrategyAll)
	assert.True(t, rc.ShouldUseGraphCache("https://cluster1.example.com"))
}

func TestRolloutConfig_UpdatePercentage(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:   RolloutStrategyPercentage,
		Percentage: 0,
		Allowlist:  make(map[string]bool),
	}

	// Nothing enabled at 0%
	enabled := 0
	for i := 0; i < 100; i++ {
		if rc.ShouldUseGraphCache(fmt.Sprintf("https://cluster%d.example.com", i)) {
			enabled++
		}
	}
	assert.Equal(t, 0, enabled)

	// Everything enabled at 100%
	rc.UpdatePercentage(100)
	enabled = 0
	for i := 0; i < 100; i++ {
		if rc.ShouldUseGraphCache(fmt.Sprintf("https://cluster%d.example.com", i)) {
			enabled++
		}
	}
	assert.Equal(t, 100, enabled)
}

func TestRolloutConfig_UpdatePercentage_Clamped(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:  RolloutStrategyPercentage,
		Allowlist: make(map[string]bool),
	}

	rc.UpdatePercentage(-10)
	assert.Equal(t, 0, rc.Percentage)

	rc.UpdatePercentage(200)
	assert.Equal(t, 100, rc.Percentage)
}

func TestRolloutConfig_AllowlistMutation(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:  RolloutStrategyAllowlist,
		Allowlist: make(map[string]bool),
	}

	server := "https://cluster1.example.com"
	assert.False(t, rc.ShouldUseGraphCache(server))

	rc.AddToAllowlist(server)
	assert.True(t, rc.ShouldUseGraphCache(server))

	rc.RemoveFromAllowlist(server)
	assert.False(t, rc.ShouldUseGraphCache(server))
}

func TestRolloutConfig_UnknownStrategy(t *testing.T) {
	rc := &RolloutConfig{
		Strategy:  "unknown",
		Allowlist: make(map[string]bool),
	}

	// Should default to true (like "all")
	assert.True(t, rc.ShouldUseGraphCache("https://cluster1.example.com"))
}
