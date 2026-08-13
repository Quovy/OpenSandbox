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

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.Instant
import java.util.concurrent.atomic.AtomicInteger

class PoolReconcilerStateTest {
    private class MutableClock(var now: Instant) {
        fun advance(amount: Duration) {
            now = now.plus(amount)
        }
    }

    @Test
    fun `recordFailures transitions to DEGRADED when windowed count reaches threshold`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state =
            ReconcileState(
                degradedThreshold = 3,
                failureWindow = Duration.ofSeconds(60),
                backoffBase = Duration.ofMillis(10),
                backoffMax = Duration.ofSeconds(1),
                clock = { clock.now },
            )
        state.recordFailure("boom-1")
        clock.advance(Duration.ofSeconds(1))
        state.recordFailure("boom-2")
        assertEquals(PoolState.HEALTHY, state.state)
        assertFalse(state.isBackoffActive())

        clock.advance(Duration.ofSeconds(1))
        state.recordFailure("boom-3")
        assertEquals(PoolState.DEGRADED, state.state)
        assertEquals(3, state.failureCount)
    }

    @Test
    fun `windowed failures trigger DEGRADED even when spread across the window`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state = ReconcileState(degradedThreshold = 3, clock = { clock.now })

        state.recordAsyncFailure("boom-1")
        clock.advance(Duration.ofSeconds(2))
        state.recordAsyncFailure("boom-2")
        assertEquals(PoolState.HEALTHY, state.state)

        // The detection window counts failures by rate, not consecutively: a gap of healthy
        // operation between failures (interleaved successes) does not reset the count.
        clock.advance(Duration.ofSeconds(2))
        state.recordAsyncFailure("boom-3")
        assertEquals(PoolState.DEGRADED, state.state)
        assertEquals(3, state.failureCount)
    }

    @Test
    fun `default degraded backoff starts at thirty seconds`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state = ReconcileState(degradedThreshold = 1, clock = { clock.now })

        state.recordFailure("boom")

        assertEquals(true, state.isBackoffActive(clock.now.plus(Duration.ofSeconds(29))))
        // The 30s backoff expires at t=30, but the failure is still inside the 60s window, so
        // the backoff is renewed (t=30 -> t=91) and the pool stays paused.
        assertEquals(true, state.isBackoffActive(clock.now.plus(Duration.ofSeconds(31))))
        assertFalse(state.isBackoffActive(clock.now.plus(Duration.ofSeconds(92))))
        assertEquals(PoolState.HEALTHY, state.state)
        assertEquals(0, state.failureCount)
        assertEquals(null, state.lastError)
    }

    @Test
    fun `sustained failures keep backoff active and recovery follows window drain`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state = ReconcileState(degradedThreshold = 1, clock = { clock.now })

        repeat(5_000) {
            clock.advance(Duration.ofSeconds(30))
            state.recordFailure("boom")
        }

        assertEquals(PoolState.DEGRADED, state.state)
        assertEquals(true, state.isBackoffActive())
        // Once failures stop, the pool stays paused until the failure window drains.
        assertFalse(state.isBackoffActive(clock.now.plus(Duration.ofHours(25))))
        assertEquals(PoolState.HEALTHY, state.state)
        assertEquals(0, state.failureCount)
    }

    @Test
    fun `backoff escalation stays capped at one day under sustained renewals`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state =
            ReconcileState(
                degradedThreshold = 1,
                failureWindow = Duration.ofDays(100),
                clock = { clock.now },
            )

        // Each failure lands just after the previous backoff window (at most one day), forcing
        // a new escalation. The 31st activation must not overflow Duration's nanosecond
        // capacity and must stay capped at backoffMax.
        repeat(40) {
            clock.advance(Duration.ofSeconds(86_401))
            state.recordAsyncFailure("boom")
        }

        assertEquals(PoolState.DEGRADED, state.state)
        assertTrue(state.isBackoffActive())
    }

    @Test
    fun `in-window failures do not advance backoff per completion`() {
        val clock = MutableClock(Instant.parse("2025-01-01T00:00:00Z"))
        val state = ReconcileState(degradedThreshold = 3, clock = { clock.now })

        repeat(3) { state.recordAsyncFailure("boom") }
        repeat(6) {
            clock.advance(Duration.ofSeconds(5))
            state.recordAsyncFailure("boom")
        }

        assertEquals(9, state.failureCount)
        // Exactly one escalation: the 30s window opened at t=0 and was renewed once at expiry
        // (t=30 -> t=90), not once per in-window failure completion.
        assertEquals(true, state.isBackoffActive(clock.now.plus(Duration.ofSeconds(59))))
        assertFalse(state.isBackoffActive(clock.now.plus(Duration.ofSeconds(61))))
    }

    @Test
    fun `reconcile skips create when current node is not primary`() {
        val stateStore = AlwaysSecondaryStore()
        val config =
            PoolConfig.builder()
                .poolName("pool-not-primary")
                .ownerId("owner-2")
                .maxIdle(1)
                .warmupConcurrency(1)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val submitted = AtomicInteger(0)

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { submitted.addAndGet(it) },
        )

        assertEquals(0, submitted.get())
        assertEquals(emptyList<String>(), stateStore.putIdleIds)
    }

    @Test
    fun `only primary owner can submit warmups for same pool`() {
        val stateStore = OwnerLockingStore()
        val primaryConfig = buildConfig(ownerId = "owner-primary", maxIdle = 1, stateStore = stateStore, poolName = "pool-owner-lock")
        val secondaryConfig = buildConfig(ownerId = "owner-secondary", maxIdle = 1, stateStore = stateStore, poolName = "pool-owner-lock")
        val state = ReconcileState(degradedThreshold = 10)
        val primarySubmissions = AtomicInteger(0)
        val secondarySubmissions = AtomicInteger(0)

        PoolReconciler.runReconcileTick(
            config = primaryConfig,
            stateStore = stateStore,
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { primarySubmissions.addAndGet(it) },
        )
        PoolReconciler.runReconcileTick(
            config = secondaryConfig,
            stateStore = stateStore,
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { secondarySubmissions.addAndGet(it) },
        )

        assertEquals(1, primarySubmissions.get())
        assertEquals(0, secondarySubmissions.get())
    }

    @Test
    fun `reconcile does not submit warmups when initial renew fails`() {
        val stateStore = RenewFailsOnFirstCallStore()
        val config = buildConfig(ownerId = "owner-1", maxIdle = 1, stateStore = stateStore, poolName = "pool-renew-first-fail")
        val state = ReconcileState(degradedThreshold = 10)
        val submitted = AtomicInteger(0)

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { submitted.addAndGet(it) },
        )

        assertEquals(0, submitted.get())
        assertEquals(emptyList<String>(), stateStore.putIdleIds)
    }

    @Test
    fun `reconcile shrinks excess idle when current node is primary`() {
        val stateStore = ShrinkStore(idleIds = listOf("id-1", "id-2", "id-3", "id-4", "id-5"))
        val config =
            PoolConfig.builder()
                .poolName("pool-shrink")
                .ownerId("owner-1")
                .maxIdle(2)
                .warmupConcurrency(2)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val discarded = mutableListOf<String>()
        val submitted = AtomicInteger(0)

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            onDiscardSandbox = { discarded += it },
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { submitted.addAndGet(it) },
        )

        assertEquals(0, submitted.get())
        assertEquals(listOf("id-1", "id-2"), discarded)
        assertEquals(listOf("id-3", "id-4", "id-5"), stateStore.idleIds)
    }

    @Test
    fun `reconcile does not shrink when current node is not primary`() {
        val stateStore = SecondaryShrinkStore(idleIds = listOf("id-1", "id-2", "id-3"))
        val config = buildConfig(ownerId = "owner-2", maxIdle = 1, stateStore = stateStore, poolName = "pool-secondary-shrink")
        val state = ReconcileState(degradedThreshold = 10)
        val discarded = mutableListOf<String>()

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            onDiscardSandbox = { discarded += it },
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { error("secondary must not submit warmups: $it") },
        )

        assertEquals(emptyList<String>(), discarded)
        assertEquals(listOf("id-1", "id-2", "id-3"), stateStore.idleIds)
    }

    @Test
    fun `reconcile stops shrinking when primary renew fails`() {
        val stateStore = ShrinkStore(idleIds = listOf("id-1", "id-2", "id-3"), renewFailuresAfter = 1)
        val config =
            PoolConfig.builder()
                .poolName("pool-shrink-renew-fails")
                .ownerId("owner-1")
                .maxIdle(0)
                .warmupConcurrency(3)
                .stateStore(stateStore)
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()
        val state = ReconcileState(degradedThreshold = 10)
        val discarded = mutableListOf<String>()

        PoolReconciler.runReconcileTick(
            config = config,
            stateStore = stateStore,
            onDiscardSandbox = { discarded += it },
            reconcileState = state,
            warmingCount = 0,
            submitWarmups = { error("shrink must not submit warmups: $it") },
        )

        assertEquals(listOf("id-1"), discarded)
        assertEquals(listOf("id-2", "id-3"), stateStore.idleIds)
    }

    private fun buildConfig(
        ownerId: String,
        maxIdle: Int,
        stateStore: PoolStateStore,
        poolName: String,
    ): PoolConfig {
        return PoolConfig.builder()
            .poolName(poolName)
            .ownerId(ownerId)
            .maxIdle(maxIdle)
            .warmupConcurrency(1)
            .stateStore(stateStore)
            .connectionConfig(ConnectionConfig.builder().build())
            .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
            .build()
    }

    private class AlwaysSecondaryStore : PoolStateStore {
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = false

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = false

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class OwnerLockingStore : PoolStateStore {
        @Volatile
        private var lockOwner: String? = null
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            val currentOwner = lockOwner
            return if (currentOwner == null || currentOwner == ownerId) {
                lockOwner = ownerId
                true
            } else {
                false
            }
        }

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = lockOwner == ownerId

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
            if (lockOwner == ownerId) {
                lockOwner = null
            }
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = putIdleIds.size)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class RenewFailsOnFirstCallStore : PoolStateStore {
        private val renewCalls = AtomicInteger(0)
        val putIdleIds = mutableListOf<String>()

        override fun tryTakeIdle(poolName: String): String? = null

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
            putIdleIds += sandboxId
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            val call = renewCalls.incrementAndGet()
            return call > 1
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = 0)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private open class ShrinkStore(
        idleIds: List<String>,
        private val renewFailuresAfter: Int? = null,
    ) : PoolStateStore {
        val idleIds = idleIds.toMutableList()
        private val renewCalls = AtomicInteger(0)

        override fun tryTakeIdle(poolName: String): String? {
            return if (idleIds.isEmpty()) null else idleIds.removeAt(0)
        }

        override fun putIdle(
            poolName: String,
            sandboxId: String,
        ) {
        }

        override fun removeIdle(
            poolName: String,
            sandboxId: String,
        ) {
            idleIds.remove(sandboxId)
        }

        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = true

        override fun renewPrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean {
            val call = renewCalls.incrementAndGet()
            return renewFailuresAfter == null || call <= renewFailuresAfter
        }

        override fun releasePrimaryLock(
            poolName: String,
            ownerId: String,
        ) {
        }

        override fun reapExpiredIdle(
            poolName: String,
            now: Instant,
        ) {
        }

        override fun snapshotCounters(poolName: String): StoreCounters = StoreCounters(idleCount = idleIds.size)

        override fun snapshotIdleEntries(poolName: String) = emptyList<com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry>()

        override fun getMaxIdle(poolName: String): Int? = null

        override fun setMaxIdle(
            poolName: String,
            maxIdle: Int,
        ) {
        }
    }

    private class SecondaryShrinkStore(
        idleIds: List<String>,
    ) : ShrinkStore(idleIds) {
        override fun tryAcquirePrimaryLock(
            poolName: String,
            ownerId: String,
            ttl: Duration,
        ): Boolean = false
    }
}
