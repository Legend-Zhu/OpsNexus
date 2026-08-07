package nodeagent

import (
	"testing"
	"time"
)

// TestStatsCache 验证 stats 短缓存：命中返回防御性拷贝、TTL 过期后未命中。
func TestStatsCache(t *testing.T) {
	a := &API{}

	if _, ok := a.cachedStats(); ok {
		t.Fatal("empty cache should miss")
	}

	resp := &StatsResp{Node: "n1", Containers: []ContainerStat{{ContainerID: "c1"}}}
	a.putStatsCache(resp)

	got, ok := a.cachedStats()
	if !ok {
		t.Fatal("fresh cache should hit")
	}
	if got.Node != "n1" || len(got.Containers) != 1 {
		t.Fatalf("bad cached copy: %+v", got)
	}
	// 防御性拷贝：修改返回值不得污染缓存。
	got.Containers[0].ContainerID = "mutated"
	if a.statsCache.Containers[0].ContainerID != "c1" {
		t.Fatal("cache mutated via returned copy")
	}

	// TTL 过期后视为未命中。
	a.statsAt = time.Now().Add(-statsCacheTTL - time.Second)
	if _, ok := a.cachedStats(); ok {
		t.Fatal("stale cache should miss")
	}
}
