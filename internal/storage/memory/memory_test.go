package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

func TestSessionStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewSessionStore()

	sess := &domain.Session{ID: "s1", Title: "title"}
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "s1" || got.Title != "title" {
		t.Fatalf("unexpected session: %+v", got)
	}

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 session, got %d", len(all))
	}
}

func TestTaskStoreOneTaskPerSession(t *testing.T) {
	ctx := context.Background()
	s := NewTaskStore()

	if err := s.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "a"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// 同一 Session 再创建第二个 Task 应被拒绝
	if err := s.Create(ctx, &domain.Task{ID: "t2", SessionID: "s1", Title: "b"}); !errors.Is(err, storage.ErrSessionHasTask) {
		t.Fatalf("want ErrSessionHasTask, got %v", err)
	}
	// 不同 Session 可以创建
	if err := s.Create(ctx, &domain.Task{ID: "t3", SessionID: "s2", Title: "c"}); err != nil {
		t.Fatalf("create under other session: %v", err)
	}
}

func TestTaskStoreListBySession(t *testing.T) {
	ctx := context.Background()
	s := NewTaskStore()

	for _, tk := range []*domain.Task{
		{ID: "t1", SessionID: "s1", Title: "a"},
		{ID: "t3", SessionID: "s2", Title: "c"},
	} {
		if err := s.Create(ctx, tk); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	got, err := s.ListBySession(ctx, "s1")
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("want 1 task (t1) under s1, got %+v", got)
	}

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStoreDelete(t *testing.T) {
	ctx := context.Background()
	ss := NewSessionStore()
	ts := NewTaskStore()

	if err := ss.Create(ctx, &domain.Session{ID: "s1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := ts.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := ts.Delete(ctx, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := ts.Get(ctx, "t1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound after task delete, got %v", err)
	}
	if err := ts.Delete(ctx, "t1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound on double delete, got %v", err)
	}

	if err := ss.Delete(ctx, "s1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := ss.Get(ctx, "s1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound after session delete, got %v", err)
	}
	if err := ss.Delete(ctx, "s1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound on double session delete, got %v", err)
	}
}
