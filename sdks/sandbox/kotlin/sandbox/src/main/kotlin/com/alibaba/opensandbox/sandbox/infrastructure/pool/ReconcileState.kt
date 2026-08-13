/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.infrastructure.pool

import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import java.time.Duration
import java.time.Instant

/**
 * Mutable state for reconcile loop: sliding-window failure detection, pool state, and exponential
 * backoff.
 *
 * Degradation is detected by failure *rate* rather than consecutive failures: every failure is
 * recorded with a timestamp and ages out of [failureWindow]. The pool enters DEGRADED and opens an
 * exponential backoff window once [degradedThreshold] failures are present inside the window.
 * Successful creates never reset the window (recovery is time-based, not success-based), and an
 * active backoff window is never cancelled by a success.
 *
 * While DEGRADED, an expired backoff window is renewed automatically whenever the failure window
 * is still hot, so the pool stays paused until the failure rate actually drops below the
 * threshold. The pool returns to HEALTHY only once the failure window drains and no backoff is
 * active.
 *
 * Thread-safe for use from reconcile worker and from pool snapshot.
 */
internal class ReconcileState(
    private val degradedThreshold: Int,
    private val failureWindow: Duration = Duration.ofSeconds(60),
    private val backoffBase: Duration = Duration.ofSeconds(30),
    private val backoffMax: Duration = Duration.ofDays(1),
    private val clock: () -> Instant = Instant::now,
) {
    @Volatile
    var failureCount: Int = 0
        private set

    @Volatile
    var state: PoolState = PoolState.HEALTHY
        private set

    @Volatile
    var lastError: String? = null
        private set

    @Volatile
    private var backoffUntil: Instant? = null

    private var backoffAttempts: Int = 0

    private val failureTimestamps: ArrayDeque<Instant> = ArrayDeque()

    /**
     * Records one independently completed warmup failure.
     *
     * Multiple tasks that finish while the same backoff window is active must not advance the
     * exponential delay once per completion: the first failure that reaches the threshold opens
     * (or, after expiry, advances) one backoff window; remaining in-flight failures only update
     * counters and the last error.
     */
    @Synchronized
    fun recordAsyncFailure(errorMessage: String?) {
        val now = clock()
        failureTimestamps.addLast(now)
        pruneExpired(now)
        lastError = errorMessage
        if (failureCount < degradedThreshold) return

        val activeUntil = backoffUntil
        if (state == PoolState.DEGRADED && activeUntil != null && now.isBefore(activeUntil)) {
            return
        }
        activateNextBackoff(now)
    }

    @Synchronized
    fun recordFailure(errorMessage: String?) {
        recordFailures(1, errorMessage)
    }

    @Synchronized
    fun recordFailures(
        count: Int,
        errorMessage: String?,
    ) {
        if (count <= 0) return
        val now = clock()
        repeat(count) { failureTimestamps.addLast(now) }
        pruneExpired(now)
        lastError = errorMessage
        if (failureCount < degradedThreshold) return

        val activeUntil = backoffUntil
        if (state == PoolState.DEGRADED && activeUntil != null && now.isBefore(activeUntil)) {
            return
        }
        activateNextBackoff(now)
    }

    /**
     * True if the reconciler should skip create attempts this tick (in backoff window).
     *
     * Advances the state machine on the read path: while DEGRADED, an expired backoff window is
     * renewed when the failure window is still hot, so the pool stays paused until the failure
     * rate falls below the threshold; once the failure window drains and no backoff is active,
     * the pool recovers to HEALTHY.
     */
    @Synchronized
    fun isBackoffActive(now: Instant = clock()): Boolean {
        pruneExpired(now)
        val until = backoffUntil
        if (state != PoolState.DEGRADED || until == null) return false
        if (now.isBefore(until)) return true
        return if (failureCount >= degradedThreshold) {
            activateNextBackoff(now)
            true
        } else {
            recover()
            false
        }
    }

    private fun activateNextBackoff(now: Instant) {
        state = PoolState.DEGRADED
        backoffAttempts++
        val exponent = (backoffAttempts - 1).coerceAtMost(30)
        val delaySeconds = backoffBase.seconds * (1L shl exponent)
        val delayMs =
            minOf(
                Duration.ofSeconds(delaySeconds).toMillis(),
                backoffMax.toMillis(),
            )
        backoffUntil = now.plusMillis(delayMs)
    }

    private fun recover() {
        state = PoolState.HEALTHY
        backoffUntil = null
        backoffAttempts = 0
        lastError = null
    }

    private fun pruneExpired(now: Instant) {
        val cutoff = now.minus(failureWindow)
        while (failureTimestamps.isNotEmpty() && failureTimestamps.first().isBefore(cutoff)) {
            failureTimestamps.removeFirst()
        }
        failureCount = failureTimestamps.size
    }
}
