package cluster

import (
	"context"
	"path/filepath"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// newInventoryTestService 跳过 Worker 探测直接落库一个集群（单条纳管操作只
// 读写 store，不需要真实 Worker）。
func newInventoryTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutCluster(&store.Cluster{Name: "dev"}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	return New(st), "dev"
}

func invStandalone(name, node string) *store.InventoryItem {
	return &store.InventoryItem{
		Name: name, Type: store.InvStandaloneContainer, Ref: name, Node: node,
		Category: "middleware", Ports: []string{"8848"},
	}
}

// TestAddInventoryItem 追加成功可见；重名被拒（ErrDuplicate）；非法条目与
// 不存在的集群均报错。
func TestAddInventoryItem(t *testing.T) {
	svc, cluster := newInventoryTestService(t)
	ctx := context.Background()

	if _, err := svc.AddInventoryItem(ctx, cluster, invStandalone("r-nacos", "node-01")); err != nil {
		t.Fatalf("add: %v", err)
	}
	rec, err := svc.GetStatic(cluster)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n := len(rec.Inventory.Items); n != 1 {
		t.Fatalf("expected 1 item, got %d", n)
	}

	// 重名
	if _, err := svc.AddInventoryItem(ctx, cluster, invStandalone("r-nacos", "node-02")); err == nil {
		t.Fatal("expected duplicate error")
	} else if _, ok := err.(ErrDuplicate); !ok {
		t.Fatalf("expected ErrDuplicate, got %T: %v", err, err)
	}

	// 非法条目（standalone 缺 node）
	if _, err := svc.AddInventoryItem(ctx, cluster, &store.InventoryItem{
		Name: "bad", Type: store.InvStandaloneContainer, Ref: "bad",
	}); err == nil {
		t.Fatal("expected validation error for missing node")
	}

	// 集群不存在
	if _, err := svc.AddInventoryItem(ctx, "nope", invStandalone("x", "n1")); err == nil {
		t.Fatal("expected ErrNotFound for missing cluster")
	} else if _, ok := err.(ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}
}

// TestUpdateInventoryItem 原位替换、改名、改名撞名被拒、条目不存在被拒。
func TestUpdateInventoryItem(t *testing.T) {
	svc, cluster := newInventoryTestService(t)
	ctx := context.Background()
	if _, err := svc.AddInventoryItem(ctx, cluster, invStandalone("r-nacos", "node-01")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := svc.AddInventoryItem(ctx, cluster, invStandalone("grafana", "node-02")); err != nil {
		t.Fatalf("add: %v", err)
	}

	// 原位替换（改名关联字段）
	upd := invStandalone("r-nacos", "node-01")
	upd.Ports = []string{"8848", "9848"}
	if _, err := svc.UpdateInventoryItem(ctx, cluster, "r-nacos", upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	rec, _ := svc.GetStatic(cluster)
	if len(rec.Inventory.Items[0].Ports) != 2 {
		t.Fatalf("update should replace in place, got %+v", rec.Inventory.Items[0])
	}

	// 改名
	if _, err := svc.UpdateInventoryItem(ctx, cluster, "r-nacos", invStandalone("nacos-2", "node-01")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	rec, _ = svc.GetStatic(cluster)
	if findItemIdx(rec.Inventory.Items, "nacos-2") < 0 || findItemIdx(rec.Inventory.Items, "r-nacos") >= 0 {
		t.Fatalf("rename failed: %+v", rec.Inventory.Items)
	}

	// 改名撞上已有条目
	if _, err := svc.UpdateInventoryItem(ctx, cluster, "nacos-2", invStandalone("grafana", "node-02")); err == nil {
		t.Fatal("expected duplicate error on rename collision")
	} else if _, ok := err.(ErrDuplicate); !ok {
		t.Fatalf("expected ErrDuplicate, got %T: %v", err, err)
	}

	// 条目不存在
	if _, err := svc.UpdateInventoryItem(ctx, cluster, "ghost", invStandalone("x", "n1")); err == nil {
		t.Fatal("expected not-found error")
	} else if _, ok := err.(ErrItemNotFound); !ok {
		t.Fatalf("expected ErrItemNotFound, got %T: %v", err, err)
	}
}

// TestDeleteInventoryItem 删除后清单缩短；条目/清单/集群不存在均 ErrItemNotFound
// 或 ErrNotFound。
func TestDeleteInventoryItem(t *testing.T) {
	svc, cluster := newInventoryTestService(t)
	ctx := context.Background()
	if _, err := svc.AddInventoryItem(ctx, cluster, invStandalone("r-nacos", "node-01")); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := svc.DeleteInventoryItem(ctx, cluster, "ghost"); err == nil {
		t.Fatal("expected ErrItemNotFound for missing item")
	} else if _, ok := err.(ErrItemNotFound); !ok {
		t.Fatalf("expected ErrItemNotFound, got %T: %v", err, err)
	}

	if _, err := svc.DeleteInventoryItem(ctx, cluster, "r-nacos"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rec, _ := svc.GetStatic(cluster)
	if len(rec.Inventory.Items) != 0 {
		t.Fatalf("expected empty inventory, got %+v", rec.Inventory.Items)
	}

	// 清单已空（Inventory 非 nil 但无条目）
	if _, err := svc.DeleteInventoryItem(ctx, cluster, "r-nacos"); err == nil {
		t.Fatal("expected ErrItemNotFound on empty inventory")
	}
}
