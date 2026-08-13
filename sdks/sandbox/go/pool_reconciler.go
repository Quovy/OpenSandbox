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
	"fmt"
	"sync"
	"time"
)

const (
	reconcileBackoffBase = 30 * time.Second
	reconcileMaxBackoff  = 24 * time.Hour
	// defaultFailureWindow is the default sliding time window over which create
	// failures are counted for degraded detection.
	defaultFailureWindow = 60 * time.Second
)

// reconcileState tracks the health and backoff state of the reconcile loop.
//
// Degradation is detected by failure *rate* rather than consecutive failures: every failure is
// recorded with a timestamp and ages out of failureWindow. The pool enters PoolDegraded and opens
// an exponential backoff window once degradedThreshold failures are present inside the window.
// Successful creates never reset the window (recovery is time-based, not success-based), and an
// active backoff window is never cancelled by a success.
//
// While PoolDegraded, an expired backoff window is renewed automatically whenever the failure
// window is still hot, so the pool stays paused until the failure rate actually drops below the
// threshold. The pool returns to PoolHealthy only once the failure window drains and no backoff
// is active.
type reconcileState struct {
	mu                sync.Mutex
	degradedThreshold int
	failureWindow     time.Duration
	failureCount      int
	failureTimes      []time.Time
	backoffAttempts   int
	backoffUntil      time.Time
	lastError         string
	healthState       PoolHealthState
	now               func() time.Time
}

// newReconcileState creates a new reconcileState with the given degraded threshold and the
// default failure window.
func newReconcileState(degradedThreshold int) *reconcileState {
	return &reconcileState{
		degradedThreshold: degradedThreshold,
		failureWindow:     defaultFailureWindow,
		healthState:       PoolHealthy,
		now:               time.Now,
	}
}

// recordFailure records a single failure. Delegates to recordFailures.
func (s *reconcileState) recordFailure(err error) {
	s.recordFailures(1, err)
}

// recordFailures records count failures in one call. Failures are recorded with the current
// timestamp and age out of the sliding failureWindow; the pool transitions to degraded and opens
// an exponential backoff window when the windowed count reaches or exceeds the degraded
// threshold. Failures recorded while a backoff window is already active do not advance the
// exponential delay (only the counter and last error are updated).
func (s *reconcileState) recordFailures(count int, err error) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for i := 0; i < count; i++ {
		s.failureTimes = append(s.failureTimes, now)
	}
	s.pruneExpired(now)
	if err != nil {
		s.lastError = err.Error()
	}
	if s.failureCount < s.degradedThreshold {
		return
	}
	if s.healthState == PoolDegraded && !s.backoffUntil.IsZero() && now.Before(s.backoffUntil) {
		return
	}
	s.activateNextBackoff(now)
}

// shouldBackoff returns true if the reconciler is in a backoff period.
//
// Advances the state machine on the read path: while PoolDegraded, an expired backoff window is
// renewed when the failure window is still hot, so the pool stays paused until the failure rate
// falls below the threshold; once the failure window drains and no backoff is active, the pool
// recovers to PoolHealthy.
func (s *reconcileState) shouldBackoff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(s.now())
}

// refreshLocked advances the state machine at the current time and reports whether create
// attempts should be suppressed: prunes the sliding failure window, renews an expired backoff
// window while the window is still hot, and recovers to healthy once the window drains.
// Callers must hold s.mu.
func (s *reconcileState) refreshLocked(now time.Time) bool {
	s.pruneExpired(now)
	if s.healthState != PoolDegraded || s.backoffUntil.IsZero() {
		return false
	}
	if now.Before(s.backoffUntil) {
		return true
	}
	if s.failureCount >= s.degradedThreshold {
		s.activateNextBackoff(now)
		return true
	}
	s.recover()
	return false
}

// snapshot returns a point-in-time view of the reconcile health state. It advances the state
// machine first (prune/renew/recover) so readers never observe a stale DEGRADED state or stale
// failure count after the window has drained.
func (s *reconcileState) snapshot() (PoolHealthState, int, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backoffActive := s.refreshLocked(s.now())
	return s.healthState, s.failureCount, backoffActive, s.lastError
}

func (s *reconcileState) activateNextBackoff(now time.Time) {
	s.healthState = PoolDegraded
	s.backoffAttempts++
	shift := s.backoffAttempts - 1
	if shift > 30 {
		shift = 30
	}
	// Cap the delay in seconds before constructing the duration: with sustained
	// renewals backoffAttempts grows without bound, and 30s << 29 already overflows
	// the int64 nanosecond range of time.Duration (wrapping negative and defeating
	// the max check below).
	maxSeconds := int64(reconcileMaxBackoff / time.Second)
	delaySeconds := int64(reconcileBackoffBase/time.Second) << shift
	if delaySeconds > maxSeconds {
		delaySeconds = maxSeconds
	}
	s.backoffUntil = now.Add(time.Duration(delaySeconds) * time.Second)
}

func (s *reconcileState) recover() {
	s.healthState = PoolHealthy
	s.backoffUntil = time.Time{}
	s.backoffAttempts = 0
	s.lastError = ""
}

func (s *reconcileState) pruneExpired(now time.Time) {
	cutoff := now.Add(-s.failureWindow)
	kept := 0
	for _, ts := range s.failureTimes {
		if !ts.Before(cutoff) {
			s.failureTimes[kept] = ts
			kept++
		}
	}
	s.failureTimes = s.failureTimes[:kept]
	s.failureCount = kept
}

// reconcileTick performs a single reconciliation pass. It is designed to be
// called periodically by the pool's background loop.
//
// Logic (follows OSEP-0005 and Kotlin/Python SDKs):
//  1. Acquire the primary lock (leader election). If it fails, return immediately.
//  2. Reap expired idle entries, killing any discarded-alive sandboxes.
//  3. If idle count exceeds maxIdle, shrink excess entries.
//  4. If a deficit exists and we are not in backoff, create sandboxes up to warmupConcurrency.
//
// The leader lock is NOT released at end of tick; it is held until TTL expires
// or renew fails. This reduces lock contention in distributed (Redis) scenarios.
// Only Shutdown releases the lock explicitly.
func reconcileTick(
	ctx context.Context,
	cfg *PoolConfig,
	store PoolStateStore,
	state *reconcileState,
	logger PoolLogger,
	createFn func(ctx context.Context, reason PooledSandboxCreateReason) (string, error),
	deleteFn func(sandboxID string),
) {
	poolName := cfg.PoolName
	ownerID := cfg.OwnerID
	lockTTL := cfg.PrimaryLockTTL

	// Step 1: Try to acquire the primary lock.
	acquired, err := store.TryAcquirePrimaryLock(ctx, poolName, ownerID, lockTTL)
	if err != nil {
		logger.Warn("reconcile: lock acquire error", "pool_name", poolName, "error", err)
		state.recordFailure(err)
		return
	}
	if !acquired {
		logger.Debug("reconcile: not primary, skipping", "pool_name", poolName)
		return
	}

	// Step 2: Reap expired idle entries.
	minTTL := cfg.AcquireMinRemainingTTL
	if minTTL > 0 {
		reapResult, reapErr := store.ReapExpiredIdleWithMinTTL(ctx, poolName, time.Now(), minTTL)
		if reapErr != nil {
			logger.Warn("reconcile: reap error", "pool_name", poolName, "error", reapErr)
		} else if reapResult != nil && len(reapResult.DiscardedAliveSandboxIDs) > 0 {
			logger.Debug("reconcile: reaped near-expiry sandboxes",
				"pool_name", poolName,
				"count", len(reapResult.DiscardedAliveSandboxIDs))
			for _, id := range reapResult.DiscardedAliveSandboxIDs {
				deleteFn(id)
			}
		}
	} else {
		if reapErr := store.ReapExpiredIdle(ctx, poolName, time.Now()); reapErr != nil {
			logger.Warn("reconcile: reap error", "pool_name", poolName, "error", reapErr)
		}
	}

	// Step 3: Snapshot counters and determine current state.
	counters, err := store.SnapshotCounters(ctx, poolName)
	if err != nil {
		logger.Warn("reconcile: snapshot error", "pool_name", poolName, "error", err)
		return
	}
	maxIdle, err := store.GetMaxIdle(ctx, poolName)
	if err != nil {
		logger.Warn("reconcile: get maxIdle error", "pool_name", poolName, "error", err)
		return
	}
	idleCount := counters.IdleCount

	// Step 4: If idle > maxIdle, shrink excess.
	if idleCount > maxIdle {
		excess := idleCount - maxIdle
		toRemove := intMin(excess, cfg.WarmupConcurrency)
		logger.Debug("reconcile: shrinking excess idle",
			"pool_name", poolName,
			"idle", idleCount,
			"max_idle", maxIdle,
			"to_remove", toRemove)
		shrinkErr := false
		removedCount := 0
		for i := 0; i < toRemove; i++ {
			renewed, renewErr := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
			if renewErr != nil || !renewed {
				logger.Warn("reconcile: lost lock during shrink", "pool_name", poolName)
				return
			}
			sandboxID, takeErr := store.TryTakeIdle(ctx, poolName)
			if takeErr != nil {
				logger.Warn("reconcile: TryTakeIdle error during shrink", "pool_name", poolName, "error", takeErr)
				state.recordFailure(takeErr)
				shrinkErr = true
				break
			}
			if sandboxID == "" {
				break
			}
			deleteFn(sandboxID)
			removedCount++
		}
		if !shrinkErr && removedCount > 0 {
			logger.Debug("reconcile: shrunk excess idle",
				"pool_name", poolName,
				"removed", removedCount)
		}
		return
	}

	// Step 5: If deficit > 0 and not in backoff, create sandboxes.
	deficit := maxIdle - idleCount
	if deficit <= 0 {
		return
	}
	if state.shouldBackoff() {
		logger.Debug("reconcile: backoff active, skipping replenish",
			"pool_name", poolName,
			"deficit", deficit)
		return
	}

	renewed, err := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
	if err != nil || !renewed {
		logger.Warn("reconcile: lost lock before create", "pool_name", poolName)
		return
	}

	toCreate := intMin(deficit, cfg.WarmupConcurrency)
	logger.Debug("reconcile: filling deficit",
		"pool_name", poolName,
		"idle", idleCount,
		"deficit", deficit,
		"to_create", toCreate)

	type createResult struct {
		sandboxID string
		err       error
	}

	results := make([]createResult, toCreate)
	var wg sync.WaitGroup
	for i := 0; i < toCreate; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = createResult{err: fmt.Errorf("panic in createFn: %v", r)}
				}
			}()
			select {
			case <-ctx.Done():
				results[idx] = createResult{err: ctx.Err()}
			default:
				id, createErr := createFn(ctx, CreateReasonWarmup)
				results[idx] = createResult{sandboxID: id, err: createErr}
			}
		}(i)
	}
	wg.Wait()

	var createdIDs []string
	var lastCreateErr error
	failCount := 0
	for _, r := range results {
		if r.err != nil {
			failCount++
			lastCreateErr = r.err
			logger.Warn("reconcile: sandbox create failed",
				"pool_name", poolName,
				"error", r.err)
		} else if r.sandboxID != "" {
			createdIDs = append(createdIDs, r.sandboxID)
		}
	}
	if failCount > 0 {
		state.recordFailures(failCount, lastCreateErr)
	}

	// Place created sandboxes into idle pool. Successful puts do not reset the failure
	// window; recovery is time-based (see reconcileState).
	for i, id := range createdIDs {
		renewed, renewErr := store.RenewPrimaryLock(ctx, poolName, ownerID, lockTTL)
		if renewErr != nil || !renewed {
			for _, orphanID := range createdIDs[i:] {
				deleteFn(orphanID)
			}
			logger.Warn("reconcile: lost lock before putIdle, killing orphans",
				"pool_name", poolName,
				"orphan_count", len(createdIDs)-i)
			return
		}
		if putErr := store.PutIdle(ctx, poolName, id); putErr != nil {
			state.recordFailure(putErr)
			// Remove potentially-stored entry and kill the current sandbox.
			_ = store.RemoveIdle(ctx, poolName, id)
			deleteFn(id)
			// Kill remaining orphans.
			for _, orphanID := range createdIDs[i+1:] {
				deleteFn(orphanID)
			}
			logger.Warn("reconcile: putIdle failed, killing orphans",
				"pool_name", poolName,
				"error", putErr,
				"orphan_count", len(createdIDs)-i)
			return
		}
	}

	if len(createdIDs) > 0 {
		logger.Debug("reconcile: created sandboxes",
			"pool_name", poolName,
			"count", len(createdIDs))
	}
}

func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
