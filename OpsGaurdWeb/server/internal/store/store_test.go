package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestClusterCRUD 集群注册表增删改查 + 持久化（重开库后数据仍在）。
func TestClusterCRUD(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// 空库
	items, err := s.ListClusters()
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 clusters, got %d", len(items))
	}

	// 写入
	now := time.Now().UTC()
	c := &Cluster{
		Name:      "dev-cluster",
		WorkerURL: "http://10.0.0.1:8080",
		MCPURL:    "http://10.0.0.1:8080/mcp",
		Token:     "secret-token",
		Desc:      "dev",
		Status:    ClusterOnline,
		LastSeen:  now,
	}
	if err := s.PutCluster(c); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 读取
	got, err := s.GetCluster("dev-cluster")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected cluster, got nil")
	}
	if got.WorkerURL != c.WorkerURL || got.Token != c.Token || got.Status != ClusterOnline {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// 不存在
	missing, err := s.GetCluster("nope")
	if err != nil || missing != nil {
		t.Fatalf("expected nil for missing, got %v err=%v", missing, err)
	}

	// 列表
	items, err = s.ListClusters()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Name != "dev-cluster" {
		t.Fatalf("unexpected list: %+v", items)
	}

	// 重开库验证持久化
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got2, err := s2.GetCluster("dev-cluster")
	if err != nil || got2 == nil {
		t.Fatalf("persistence lost: got=%v err=%v", got2, err)
	}
	if got2.Token != "secret-token" {
		t.Fatalf("token not persisted: %q", got2.Token)
	}

	// 删除
	if err := s2.DeleteCluster("dev-cluster"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, _ := s2.GetCluster("dev-cluster")
	if gone != nil {
		t.Fatal("expected deletion to remove cluster")
	}
}

// TestNextSeq 序列号单调递增且跨实例不重置（单实例约束下进程内唯一）。
func TestNextSeq(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ogw-seq"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	a, err := s.NextSeq("alert")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	b, err := s.NextSeq("alert")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if a != 1 || b != 2 {
		t.Fatalf("expected 1,2 got %d,%d", a, b)
	}
	// 不同类型序列独立
	c, _ := s.NextSeq("event")
	if c != 1 {
		t.Fatalf("expected event seq 1, got %d", c)
	}
}

// TestMigrationVersion 新库自动升到 schemaVersion。
func TestMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ogw-mig"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	v, err := s.getVersion()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("expected version %d, got %d", schemaVersion, v)
	}
}
