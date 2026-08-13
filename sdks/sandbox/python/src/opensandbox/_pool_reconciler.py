#
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""Sandbox pool reconciliation logic."""

from __future__ import annotations

import logging
from collections.abc import Callable
from concurrent.futures import Executor, wait
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from opensandbox.pool_types import (
    PoolConfig,
    PoolState,
    PoolStateStore,
)
from opensandbox.pool_types import (
    reap_expired_idle_with_min_ttl as _reap_expired_idle_with_min_ttl,
)

logger = logging.getLogger(__name__)


@dataclass
class ReconcileState:
    """Sliding-window failure detection, pool state, and exponential backoff.

    Degradation is detected by failure *rate* rather than consecutive failures: every failure is
    recorded with a timestamp and ages out of ``failure_window``. The pool enters DEGRADED and
    opens an exponential backoff window once ``degraded_threshold`` failures are present inside
    the window. Successful creates never reset the window (recovery is time-based, not
    success-based), and an active backoff window is never cancelled by a success.

    While DEGRADED, an expired backoff window is renewed automatically whenever the failure
    window is still hot, so the pool stays paused until the failure rate actually drops below the
    threshold. The pool returns to HEALTHY only once the failure window drains and no backoff is
    active.
    """

    degraded_threshold: int
    failure_window: timedelta = timedelta(seconds=60)
    backoff_base: timedelta = timedelta(seconds=30)
    backoff_max: timedelta = timedelta(days=1)
    failure_count: int = 0
    state: PoolState = PoolState.HEALTHY
    last_error: str | None = None
    backoff_until: datetime | None = None
    backoff_attempts: int = 0
    _failure_timestamps: list[datetime] = field(default_factory=list, repr=False)

    def record_failure(self, error_message: str | None) -> None:
        self.record_failures(1, error_message)

    def record_failures(self, count: int, error_message: str | None) -> None:
        if count <= 0:
            return
        now = datetime.now(timezone.utc)
        self._failure_timestamps.extend([now] * count)
        self._prune_expired(now)
        self.last_error = error_message
        if self.failure_count < self.degraded_threshold:
            return
        if (
            self.state == PoolState.DEGRADED
            and self.backoff_until is not None
            and now < self.backoff_until
        ):
            return
        self._activate_next_backoff(now)

    def is_backoff_active(self, now: datetime | None = None) -> bool:
        now = now or datetime.now(timezone.utc)
        self._prune_expired(now)
        until = self.backoff_until
        if self.state != PoolState.DEGRADED or until is None:
            return False
        if now < until:
            return True
        if self.failure_count >= self.degraded_threshold:
            self._activate_next_backoff(now)
            return True
        self._recover()
        return False

    def _activate_next_backoff(self, now: datetime) -> None:
        self.state = PoolState.DEGRADED
        self.backoff_attempts += 1
        exponent = min(self.backoff_attempts - 1, 30)
        delay = min(
            self.backoff_base.total_seconds() * (1 << exponent),
            self.backoff_max.total_seconds(),
        )
        self.backoff_until = now + timedelta(seconds=delay)

    def _recover(self) -> None:
        self.state = PoolState.HEALTHY
        self.backoff_until = None
        self.backoff_attempts = 0
        self.last_error = None

    def _prune_expired(self, now: datetime) -> None:
        cutoff = now - self.failure_window
        self._failure_timestamps = [
            ts for ts in self._failure_timestamps if not ts < cutoff
        ]
        self.failure_count = len(self._failure_timestamps)


def run_reconcile_tick(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    create_one: Callable[[], str | None],
    on_discard_sandbox: Callable[[str], None],
    reconcile_state: ReconcileState,
    warmup_executor: Executor,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl

    if not state_store.try_acquire_primary_lock(pool_name, owner_id, ttl):
        logger.debug(f"Reconcile skip (not primary): pool_name={pool_name}")
        return
    _run_primary_replenish_once(
        config=config,
        state_store=state_store,
        create_one=create_one,
        on_discard_sandbox=on_discard_sandbox,
        reconcile_state=reconcile_state,
        warmup_executor=warmup_executor,
    )


def _run_primary_replenish_once(
    *,
    config: PoolConfig,
    state_store: PoolStateStore,
    create_one: Callable[[], str | None],
    on_discard_sandbox: Callable[[str], None],
    reconcile_state: ReconcileState,
    warmup_executor: Executor,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    now = datetime.now(timezone.utc)

    discarded_alive = _reap_expired_idle_with_min_ttl(
        state_store, pool_name, now, config.acquire_min_remaining_ttl
    )
    for sandbox_id in discarded_alive:
        # Reaped near-expiry but server-side TTL has not yet elapsed; kill so the live
        # sandbox does not linger past its pool membership and consume quota.
        on_discard_sandbox(sandbox_id)
    counters = state_store.snapshot_counters(pool_name)
    excess = max(0, counters.idle_count - config.max_idle)
    to_remove = min(excess, int(config.warmup_concurrency or 1))
    if to_remove > 0:
        _shrink_excess_idle(config, state_store, on_discard_sandbox, to_remove)
        return

    deficit = max(0, config.max_idle - counters.idle_count)
    to_create = min(deficit, int(config.warmup_concurrency or 1))
    if to_create == 0 or reconcile_state.is_backoff_active(now):
        state_store.renew_primary_lock(pool_name, owner_id, ttl)
        return

    if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
        return

    futures = [warmup_executor.submit(create_one) for _ in range(to_create)]
    wait(futures)
    created_ids: list[str] = []
    failure_count = 0
    last_error: str | None = None
    for future in futures:
        try:
            sandbox_id = future.result()
            if sandbox_id is not None:
                created_ids.append(sandbox_id)
            else:
                failure_count += 1
                last_error = None
        except Exception as exc:
            failure_count += 1
            last_error = str(exc)
    reconcile_state.record_failures(failure_count, last_error)

    created = 0
    for index, sandbox_id in enumerate(created_ids):
        if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
            for orphaned_id in created_ids[index:]:
                _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                f"Reconcile lost primary lock before put_idle; dropped {len(created_ids) - index} newly created sandbox(es): pool_name={pool_name}"
            )
            return
        try:
            state_store.put_idle(pool_name, sandbox_id)
            created += 1
        except Exception as exc:
            reconcile_state.record_failure(str(exc))
            for orphaned_id in created_ids[index:]:
                try:
                    state_store.remove_idle(pool_name, orphaned_id)
                except Exception:
                    pass
                _discard(on_discard_sandbox, orphaned_id)
            logger.warning(
                f"Reconcile put_idle failed; dropped {len(created_ids) - index} newly created sandbox(es): pool_name={pool_name} error={exc}"
            )
            return
    if created > 0:
        logger.debug(f"Reconcile created {created} sandboxes: pool_name={pool_name}")


def _shrink_excess_idle(
    config: PoolConfig,
    state_store: PoolStateStore,
    on_discard_sandbox: Callable[[str], None],
    to_remove: int,
) -> None:
    pool_name = config.pool_name
    owner_id = str(config.owner_id)
    ttl = config.primary_lock_ttl
    removed = 0
    for _ in range(to_remove):
        if not state_store.renew_primary_lock(pool_name, owner_id, ttl):
            logger.warning(
                f"Reconcile lost primary lock before shrinking idle: pool_name={pool_name} removed={removed}"
            )
            return
        sandbox_id = state_store.try_take_idle(pool_name)
        if sandbox_id is None:
            return
        _discard(on_discard_sandbox, sandbox_id)
        removed += 1

    state_store.renew_primary_lock(pool_name, owner_id, ttl)
    logger.debug(f"Reconcile shrunk {removed} idle sandbox(es): pool_name={pool_name}")


def _discard(on_discard_sandbox: Callable[[str], None], sandbox_id: str) -> None:
    try:
        on_discard_sandbox(sandbox_id)
    except Exception as exc:
        logger.warning(
            f"Reconcile sandbox cleanup failed: sandbox_id={sandbox_id} error={exc}"
        )
