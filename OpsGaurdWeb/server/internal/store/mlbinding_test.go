package store

import (
	"testing"
	"time"
)

func TestScenarioBindingCRUD(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if b, err := s.GetScenarioBinding("chat"); err != nil || b != nil {
		t.Fatalf("missing binding should be nil,nil: %v %v", b, err)
	}
	if err := s.SaveScenarioBinding(&ScenarioBinding{
		Scenario: "chat", Model: "gpt-4o", UpdatedBy: "admin", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetScenarioBinding("chat")
	if err != nil || b == nil || b.Model != "gpt-4o" {
		t.Fatalf("get binding: %v %v", b, err)
	}
	items, err := s.ListScenarioBindings()
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %v", items, err)
	}
	if err := s.DeleteScenarioBinding("chat"); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.GetScenarioBinding("chat"); b != nil {
		t.Fatal("binding should be deleted")
	}
}

func TestMLBudgetNotifiedDedup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	month := "2026-08"
	if err := s.SaveMLBudget(&MLBudget{
		Month: month, Currency: "CNY", LimitMinor: 100_000_000, WarnAt: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	// 同档位只补记一次（并发/重复触发幂等）
	first, err := s.MarkBudgetNotified(month, 0.8)
	if err != nil || !first {
		t.Fatalf("first mark: %v %v", first, err)
	}
	second, err := s.MarkBudgetNotified(month, 0.8)
	if err != nil || second {
		t.Fatalf("duplicate tier should return false: %v %v", second, err)
	}
	third, err := s.MarkBudgetNotified(month, 1.0)
	if err != nil || !third {
		t.Fatalf("second tier: %v %v", third, err)
	}
	b, err := s.GetMLBudget(month)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Notified) != 2 || b.Notified[0] != 0.8 || b.Notified[1] != 1.0 {
		t.Fatalf("notified = %v", b.Notified)
	}

	// 列表与删除
	list, err := s.ListMLBudgets()
	if err != nil || len(list) != 1 {
		t.Fatalf("list budgets: %v %v", list, err)
	}
	if err := s.DeleteMLBudget(month); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMLBudget(month); err == nil {
		t.Fatal("delete missing should fail")
	}
}
