// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockLockStore wraps InMemoryPoolStateStore but overrides lock behavior for testing.
type mockLockStore struct {
	*InMemoryPoolStateStore
	acquireLockFn func(ctx context.Context, poolName, ownerID string, ttl time.Duration) (bool, error)
	renewLockFn   func(ctx context.Context, poolName, ownerID string, ttl time.Duration) (bool, error)
}

func (m *mockLockStore) TryAcquirePrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	if m.acquireLockFn != nil {
		return m.acquireLockFn(ctx, poolName, ownerID, ttl)
	}
	return m.InMemoryPoolStateStore.TryAcquirePrimaryLock(ctx, poolName, ownerID, ttl)
}

func (m *mockLockStore) RenewPrimaryLock(ctx context.Context, poolName string, ownerID string, ttl time.Duration) (bool, error) {
	if m.renewLockFn != nil {
		return m.renewLockFn(ctx, poolName, ownerID, ttl)
	}
	return m.InMemoryPoolStateStore.RenewPrimaryLock(ctx, poolName, ownerID, ttl)
}

// --- Test helpers ---

var testLogger PoolLogger = noopPoolLogger{}

func defaultTestPoolConfig(store PoolStateStore) *PoolConfig {
	return &PoolConfig{
		PoolName:               "test-pool",
		OwnerID:                "owner-1",
		MaxIdle:                5,
		WarmupConcurrency:      3,
		PrimaryLockTTL:         10 * time.Second,
		ReconcileInterval:      1 * time.Second,
		DegradedThreshold:      3,
		StateStore:             store,
		AcquireMinRemainingTTL: 5 * time.Minute,
	}
}

func TestReconciler_FillDeficit(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 5)
	cfg := defaultTestPoolConfig(store)
	state := newReconcileState(cfg.DegradedThreshold)

	var created []string
	var mu sync.Mutex
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		id := fmt.Sprintf("sbx-%d", len(created)+1)
		created = append(created, id)
		return id, nil
	}
	deleteFn := func(sandboxID string) {}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	mu.Lock()
	createdCount := len(created)
	mu.Unlock()

	// WarmupConcurrency is 3, deficit is 5, so should create min(5,3) = 3
	if createdCount != 3 {
		assert.Fail(t, fmt.Sprintf("expected 3 sandboxes created, got %d", createdCount))
	}

	counters, err := store.SnapshotCounters(context.Background(), "test-pool")
	require.NoError(t, err)
	if counters.IdleCount != 3 {
		assert.Fail(t, fmt.Sprintf("expected idle count 3, got %d", counters.IdleCount))
	}
}

func TestReconciler_WarmupConcurrency(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 10)
	cfg := defaultTestPoolConfig(store)
	cfg.WarmupConcurrency = 2
	state := newReconcileState(cfg.DegradedThreshold)

	var createCount atomic.Int32
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		n := createCount.Add(1)
		return fmt.Sprintf("sbx-%d", n), nil
	}
	deleteFn := func(sandboxID string) {}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	// WarmupConcurrency is 2, deficit is 10, so only 2 should be created per tick
	if createCount.Load() != 2 {
		assert.Fail(t, fmt.Sprintf("expected 2 sandboxes created (warmupConcurrency cap), got %d", createCount.Load()))
	}
}

func TestReconciler_NoDeficit(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 3)
	cfg := defaultTestPoolConfig(store)
	state := newReconcileState(cfg.DegradedThreshold)

	// Pre-fill idle to maxIdle
	for i := 0; i < 3; i++ {
		_ = store.PutIdle(context.Background(), "test-pool", fmt.Sprintf("sbx-%d", i))
	}

	var createCount atomic.Int32
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		createCount.Add(1)
		return "sbx-new", nil
	}
	deleteFn := func(sandboxID string) {}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	if createCount.Load() != 0 {
		assert.Fail(t, fmt.Sprintf("expected 0 creates when no deficit, got %d", createCount.Load()))
	}
}

func TestReconciler_LockFailure(t *testing.T) {
	inner := NewInMemoryPoolStateStore()
	store := &mockLockStore{
		InMemoryPoolStateStore: inner,
		acquireLockFn: func(ctx context.Context, poolName, ownerID string, ttl time.Duration) (bool, error) {
			return false, nil
		},
	}
	_ = inner.SetMaxIdle(context.Background(), "test-pool", 5)
	cfg := defaultTestPoolConfig(store)
	state := newReconcileState(cfg.DegradedThreshold)

	var createCount atomic.Int32
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		createCount.Add(1)
		return "sbx-new", nil
	}
	deleteFn := func(sandboxID string) {}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	if createCount.Load() != 0 {
		assert.Fail(t, fmt.Sprintf("expected no creates when lock fails, got %d", createCount.Load()))
	}
}

func TestReconciler_ExponentialBackoff(t *testing.T) {
	state := newReconcileState(2)

	// First two failures should trigger degraded + backoff
	state.recordFailure(errors.New("fail-1"))
	state.recordFailure(errors.New("fail-2"))

	healthState, failureCount, backoffActive, lastError := state.snapshot()
	if healthState != PoolDegraded {
		assert.Fail(t, fmt.Sprintf("expected PoolDegraded, got %v", healthState))
	}
	if failureCount != 2 {
		assert.Fail(t, fmt.Sprintf("expected failureCount 2, got %d", failureCount))
	}
	if !backoffActive {
		assert.Fail(t, "expected backoff to be active")
	}
	if lastError != "fail-2" {
		assert.Fail(t, fmt.Sprintf("expected lastError 'fail-2', got %q", lastError))
	}

	// shouldBackoff should return true
	if !state.shouldBackoff() {
		assert.Fail(t, "expected shouldBackoff() to be true")
	}

	// Verify backoff formula: first backoff attempt => 30s * 2^0 = 30s
	state2 := newReconcileState(1)
	state2.recordFailure(errors.New("single-fail"))
	state2.mu.Lock()
	backoffDuration := state2.backoffUntil.Sub(time.Now())
	state2.mu.Unlock()
	// Should be approximately 30s (within some tolerance for test execution)
	if backoffDuration < 29*time.Second || backoffDuration > 31*time.Second {
		assert.Fail(t, fmt.Sprintf("expected ~30s backoff, got %v", backoffDuration))
	}

	// Escalation via recordFailures with batch count:
	// threshold=1, recordFailures(1) => backoffAttempts=1 (30s)
	// Then, once the first window expires while the failure window is still hot,
	// recordFailures(1) again => backoffAttempts=2 (60s).
	// This simulates two consecutive failing ticks (each calling recordFailures once).
	state3 := newReconcileState(1)
	state3.recordFailures(1, errors.New("tick-1-fail")) // backoffAttempts=1, backoff=30s
	state3.mu.Lock()
	state3.backoffUntil = time.Now().Add(-time.Second) // first backoff window expired
	state3.mu.Unlock()
	state3.recordFailures(1, errors.New("tick-2-fail")) // backoffAttempts=2, backoff=60s
	state3.mu.Lock()
	backoffDuration2 := state3.backoffUntil.Sub(time.Now())
	state3.mu.Unlock()
	if backoffDuration2 < 59*time.Second || backoffDuration2 > 61*time.Second {
		assert.Fail(t, fmt.Sprintf("expected ~60s backoff on second attempt, got %v", backoffDuration2))
	}
}

func TestReconciler_RecoveryAfterWindowDrain(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	state := newReconcileState(2)
	state.now = func() time.Time { return now }
	state.failureWindow = time.Minute

	// Drive into degraded state
	state.recordFailure(errors.New("fail-1"))
	state.recordFailure(errors.New("fail-2"))

	healthState, _, _, _ := state.snapshot()
	if healthState != PoolDegraded {
		assert.Fail(t, fmt.Sprintf("expected PoolDegraded before recovery, got %v", healthState))
	}

	// The 30s backoff expires while the failure window is still hot: the pool stays
	// paused (renewed), not recovered. Successes do not reset anything.
	now = now.Add(31 * time.Second)
	if !state.shouldBackoff() {
		assert.Fail(t, "expected backoff to stay active while the failure window is hot")
	}

	// Once the failure window drains and the renewed backoff window (t=31+60s) expires,
	// the pool recovers to healthy.
	now = now.Add(70 * time.Second)
	if state.shouldBackoff() {
		assert.Fail(t, "expected shouldBackoff() to be false after window drain")
	}

	healthState, failureCount, backoffActive, lastError := state.snapshot()
	if healthState != PoolHealthy {
		assert.Fail(t, fmt.Sprintf("expected PoolHealthy after window drain, got %v", healthState))
	}
	if failureCount != 0 {
		assert.Fail(t, fmt.Sprintf("expected failureCount 0 after recovery, got %d", failureCount))
	}
	if backoffActive {
		assert.Fail(t, "expected backoff to be inactive after recovery")
	}
	if lastError != "" {
		assert.Fail(t, fmt.Sprintf("expected empty lastError after recovery, got %q", lastError))
	}
}

func TestReconciler_WindowedFailuresTriggerDegraded(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	state := newReconcileState(3)
	state.now = func() time.Time { return now }
	state.failureWindow = time.Minute

	state.recordFailure(errors.New("boom-1"))
	now = now.Add(2 * time.Second)
	state.recordFailure(errors.New("boom-2"))
	healthState, _, _, _ := state.snapshot()
	if healthState != PoolHealthy {
		assert.Fail(t, fmt.Sprintf("expected PoolHealthy after two failures, got %v", healthState))
	}

	// Failures spread across the window count by rate, not consecutively: gaps of
	// healthy operation between failures do not reset the count.
	now = now.Add(2 * time.Second)
	state.recordFailure(errors.New("boom-3"))
	healthState, failureCount, _, _ := state.snapshot()
	if healthState != PoolDegraded {
		assert.Fail(t, fmt.Sprintf("expected PoolDegraded after three windowed failures, got %v", healthState))
	}
	if failureCount != 3 {
		assert.Fail(t, fmt.Sprintf("expected failureCount 3, got %d", failureCount))
	}
}

func TestReconciler_InterleavedCreatesTriggerDegraded(t *testing.T) {
	// Regression: with the old consecutive-failure counter, every successful warmup reset
	// the failure count, so a pool with interleaved successes and failures never degraded.
	// Detection must be rate-based over a sliding window instead.
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 5)
	cfg := defaultTestPoolConfig(store)
	cfg.WarmupConcurrency = 4
	cfg.DegradedThreshold = 3
	state := newReconcileState(cfg.DegradedThreshold)

	var createCount atomic.Int32
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		n := createCount.Add(1)
		if n%2 == 0 {
			return "", errors.New("create boom")
		}
		return fmt.Sprintf("sbx-%d", n), nil
	}
	deleteFn := func(sandboxID string) {}

	// Tick 1: 2 successes + 2 failures (batch count reaches 2). Tick 2 adds failures
	// while successful creates are interleaved; the windowed count crosses the threshold
	// and degrades the pool. Tick 3 must skip creation due to backoff.
	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)
	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)
	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	healthState, failureCount, backoffActive, _ := state.snapshot()
	if healthState != PoolDegraded {
		assert.Fail(t, fmt.Sprintf("expected PoolDegraded with interleaved successes, got %v", healthState))
	}
	if failureCount < 3 {
		assert.Fail(t, fmt.Sprintf("expected windowed failureCount >= 3, got %d", failureCount))
	}
	if !backoffActive {
		assert.Fail(t, "expected backoff to be active")
	}
}

func TestReconciler_BackoffCapDoesNotOverflow(t *testing.T) {
	state := newReconcileState(1)
	state.failureWindow = 100 * 24 * time.Hour

	// Sustained renewals grow backoffAttempts without bound; the delay must be capped at
	// one day instead of overflowing time.Duration (which wraps negative and moves
	// backoffUntil into the past).
	for i := 0; i < 40; i++ {
		state.mu.Lock()
		state.backoffUntil = time.Now().Add(-time.Second) // previous window expired
		state.mu.Unlock()
		state.recordFailures(1, errors.New("boom"))
	}

	state.mu.Lock()
	delay := state.backoffUntil.Sub(time.Now())
	state.mu.Unlock()
	if delay <= 0 || delay > reconcileMaxBackoff {
		assert.Fail(t, fmt.Sprintf("expected backoff capped at 24h in the future, got %v", delay))
	}
	if !state.shouldBackoff() {
		assert.Fail(t, "expected backoff to be active")
	}
}

func TestReconciler_ShrinkExcess(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 2)
	cfg := defaultTestPoolConfig(store)
	cfg.WarmupConcurrency = 3
	state := newReconcileState(cfg.DegradedThreshold)

	// Pre-fill idle with more than maxIdle
	for i := 0; i < 5; i++ {
		_ = store.PutIdle(context.Background(), "test-pool", fmt.Sprintf("sbx-%d", i))
	}

	var deleted []string
	var mu sync.Mutex
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		return "", errors.New("should not create")
	}
	deleteFn := func(sandboxID string) {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, sandboxID)
	}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	mu.Lock()
	deletedCount := len(deleted)
	mu.Unlock()

	// Excess is 3, warmupConcurrency is 3, so should remove min(3,3) = 3
	if deletedCount != 3 {
		assert.Fail(t, fmt.Sprintf("expected 3 sandboxes deleted (shrink excess), got %d", deletedCount))
	}

	counters, err := store.SnapshotCounters(context.Background(), "test-pool")
	require.NoError(t, err)
	if counters.IdleCount != 2 {
		assert.Fail(t, fmt.Sprintf("expected idle count 2 after shrink, got %d", counters.IdleCount))
	}
}

func TestReconciler_ReapKillsDiscardedAlive(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 5)
	_ = store.SetIdleEntryTTL(context.Background(), "test-pool", 1*time.Minute)
	cfg := defaultTestPoolConfig(store)
	cfg.AcquireMinRemainingTTL = 30 * time.Second
	state := newReconcileState(cfg.DegradedThreshold)

	// Directly manipulate idle entries to insert one below the min TTL threshold.
	// Use PutIdle first to set up the pool, then override entries for test precision.
	_ = store.PutIdle(context.Background(), "test-pool", "sbx-healthy")

	// Insert a near-expiry entry by manipulating the internal state.
	// The InMemoryPoolStateStore's PutIdle uses SetIdleEntryTTL which we set to 1 min.
	// We need an entry with only 10s remaining which is below the 30s threshold.
	// We'll use SetIdleEntryTTL to a very short duration for this insertion.
	_ = store.SetIdleEntryTTL(context.Background(), "test-pool", 10*time.Second)
	_ = store.PutIdle(context.Background(), "test-pool", "sbx-expiring-soon")
	// Restore the TTL so future puts are normal.
	_ = store.SetIdleEntryTTL(context.Background(), "test-pool", 1*time.Minute)

	var deleted []string
	var mu sync.Mutex
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		return fmt.Sprintf("sbx-new-%d", time.Now().UnixNano()), nil
	}
	deleteFn := func(sandboxID string) {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, sandboxID)
	}

	reconcileTick(context.Background(), cfg, store, state, testLogger, createFn, deleteFn)

	mu.Lock()
	deletedCopy := make([]string, len(deleted))
	copy(deletedCopy, deleted)
	mu.Unlock()

	// The expiring-soon entry should be reaped and deleted
	found := false
	for _, id := range deletedCopy {
		if id == "sbx-expiring-soon" {
			found = true
			break
		}
	}
	if !found {
		assert.Fail(t, fmt.Sprintf("expected 'sbx-expiring-soon' to be killed via deleteFn, deleted: %v", deletedCopy))
	}
}

func TestReconciler_ContextCancellation(t *testing.T) {
	store := NewInMemoryPoolStateStore()
	_ = store.SetMaxIdle(context.Background(), "test-pool", 10)
	cfg := defaultTestPoolConfig(store)
	cfg.WarmupConcurrency = 5
	state := newReconcileState(cfg.DegradedThreshold)

	ctx, cancel := context.WithCancel(context.Background())

	var createCount atomic.Int32
	createFn := func(ctx context.Context, reason PooledSandboxCreateReason) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			n := createCount.Add(1)
			// Cancel after first creation to simulate mid-tick cancellation
			if n == 1 {
				cancel()
			}
			return fmt.Sprintf("sbx-%d", n), nil
		}
	}
	deleteFn := func(sandboxID string) {}

	// Should not panic even with cancelled context
	reconcileTick(ctx, cfg, store, state, testLogger, createFn, deleteFn)

	// The tick should handle cancellation gracefully without panicking.
	// Some sandboxes may or may not have been created depending on goroutine scheduling.
	// The key assertion is that we reach here without panicking.
	healthState, _, _, _ := state.snapshot()
	if healthState != PoolHealthy && healthState != PoolDegraded {
		assert.Fail(t, fmt.Sprintf("unexpected health state: %v", healthState))
	}
}
