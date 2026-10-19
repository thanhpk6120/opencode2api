package gateway

import (
	"testing"
	"time"

	"opencode2api/internal/config"
)

// lastUsedAt drives the overview "current egress" marker, so a transport must
// report zero before its first attempt and a monotonically newer time after.
func TestProxyLastUsedIsRecorded(t *testing.T) {
	pool, err := newTransportPool([]string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, config.PerformanceConfig{}, 0, config.ProxyModeRoundRobin)
	if err != nil {
		t.Fatalf("newTransportPool: %v", err)
	}
	first, second := pool.items[0], pool.items[1]
	if !first.lastUsedAt().IsZero() || !second.lastUsedAt().IsZero() {
		t.Fatal("a transport reported use before any attempt")
	}

	first.touch()
	time.Sleep(time.Millisecond)
	second.touch()

	if !first.lastUsedAt().Before(second.lastUsedAt()) {
		t.Fatalf("last used not monotonic: first=%v second=%v", first.lastUsedAt(), second.lastUsedAt())
	}
	if got := second.lastUsedAt(); got.IsZero() || time.Since(got) > time.Minute {
		t.Fatalf("unexpected timestamp: %v", got)
	}
}
