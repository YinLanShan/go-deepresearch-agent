package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// TestPGStateConcurrentCommitNoLostUpdate 验证并发 Commit 不丢更新。
//
// 采用 SELECT ... FOR UPDATE 悲观锁：并发提交会串行化，
// 每条 Result 都保留、Version 恰好等于提交次数。
//
// 若实现缺少行锁，并发会出现典型的读-改-写丢失：
// 多个事务读到同一 version，各自 +1 写回，最终版本小于提交次数。
func TestPGStateConcurrentCommitNoLostUpdate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 100)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量同时起跑，放大竞争
			err := store.Commit(ctx, task.ID, researchstate.CommitInput{
				Results: []researchstate.ResultInput{{
					AssignmentID: assignmentID(i + 1),
					Content:      fmt.Sprintf("并发证据-%d", i),
				}},
				FindingContent: fmt.Sprintf("并发过程-%d", i),
			})
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent commit failed: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// 每一条 Result 都必须保留（没有互相覆盖）。
	if len(got.Results) != workers {
		t.Fatalf("results = %d, want %d (lost updates)", len(got.Results), workers)
	}
	if len(got.Findings) != workers {
		t.Fatalf("findings = %d, want %d", len(got.Findings), workers)
	}
	// Version 恰好等于提交次数。
	if got.Version != int64(workers) {
		t.Fatalf("version = %d, want %d (lost update)", got.Version, workers)
	}
	// CommitLog：每次 1 条 result + 1 条 finding。
	if len(got.CommitLog) != workers*2 {
		t.Fatalf("commit log = %d, want %d", len(got.CommitLog), workers*2)
	}
	// 每条 Result 的 AssignmentID 应互不相同且齐全。
	seen := make(map[string]struct{}, workers)
	for _, r := range got.Results {
		seen[r.AssignmentID] = struct{}{}
	}
	for i := 0; i < workers; i++ {
		if _, ok := seen[assignmentID(i+1)]; !ok {
			t.Fatalf("assignment %s missing from results", assignmentID(i+1))
		}
	}
}

// TestPGStateConcurrentCommitAndRevoke 验证并发 Commit 与 Revoke 互不破坏。
//
// Revoke 与 Commit 都持同一行的 FOR UPDATE 锁，因此串行执行；
// 撤销命中被锁定的 Assignment，提交的其他 Assignment 不受影响。
func TestPGStateConcurrentCommitAndRevoke(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 101)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 先提交一条作为撤销目标。
	target := assignmentID(1)
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: target, Content: "撤销目标"}},
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers+1)
	start := make(chan struct{})

	// 1 个撤销 + 若干提交同时进行。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := store.RevokeResult(ctx, task.ID, target, "并发撤销"); err != nil {
			errs <- fmt.Errorf("revoke: %w", err)
		}
	}()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := store.Commit(ctx, task.ID, researchstate.CommitInput{
				Results: []researchstate.ResultInput{{
					AssignmentID: assignmentID(100 + i),
					Content:      fmt.Sprintf("并发证据-%d", i),
				}},
			})
			if err != nil {
				errs <- fmt.Errorf("commit %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent op failed: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 撤销目标应已消失，其余 6 条保留。
	if len(got.Results) != workers {
		t.Fatalf("results = %d, want %d", len(got.Results), workers)
	}
	for _, r := range got.Results {
		if r.AssignmentID == target {
			t.Fatal("revoked result should be gone")
		}
	}
	// 版本：1 次 seed commit + 1 次 revoke + workers 次 commit。
	wantVersion := int64(1 + 1 + workers)
	if got.Version != wantVersion {
		t.Fatalf("version = %d, want %d", got.Version, wantVersion)
	}
}

// TestPGStateConcurrentAppendResult 验证并发 AppendResult 不丢更新。
func TestPGStateConcurrentAppendResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 102)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := store.AppendResult(ctx, task.ID, researchstate.CommitInput{
				Results: []researchstate.ResultInput{{
					AssignmentID: assignmentID(200 + i),
					Content:      fmt.Sprintf("迟到产物-%d", i),
				}},
			})
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent append failed: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != workers {
		t.Fatalf("results = %d, want %d (lost updates)", len(got.Results), workers)
	}
	if got.Version != int64(workers) {
		t.Fatalf("version = %d, want %d", got.Version, workers)
	}
	// turn 应覆盖 1..workers 且不重复。
	turns := make(map[int]struct{}, workers)
	for _, r := range got.Results {
		turns[r.Turn] = struct{}{}
	}
	for i := 1; i <= workers; i++ {
		if _, ok := turns[i]; !ok {
			t.Fatalf("turn %d missing: %v", i, turns)
		}
	}
}

// TestPGStateConcurrentAccumulateInterruptedContext 验证并发累积中断原文不丢内容。
//
// 实现用 SQL 的 `interrupted_context || $2` 原子拼接，
// 因此所有片段都应保留（顺序可能不同，但内容不丢）。
func TestPGStateConcurrentAccumulateInterruptedContext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 103)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			piece := fmt.Sprintf("[%d]", i)
			if err := store.AccumulateInterruptedContext(ctx, task.ID, piece); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent accumulate failed: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 每个片段都必须出现恰好一次。
	text := got.InterruptedContext
	for i := 0; i < workers; i++ {
		piece := fmt.Sprintf("[%d]", i)
		count := 0
		for j := 0; j+len(piece) <= len(text); j++ {
			if text[j:j+len(piece)] == piece {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("piece %q appears %d times in %q", piece, count, text)
		}
	}
}
