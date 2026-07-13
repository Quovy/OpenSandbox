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
"""End-to-end wiring tests for Sandbox.mount / Sandbox.umount syntax sugar."""

from __future__ import annotations

from typing import Any

import pytest

from opensandbox.exceptions import MountFailedException
from opensandbox.models.execd import (
    Execution,
    ExecutionError,
    ExecutionLogs,
    OutputMessage,
)
from opensandbox.models.filesystem import WriteEntry
from opensandbox.mount import NfsMountOptions, OssfsMountOptions, OssfsVersion


class _AsyncCommandsStub:
    def __init__(self, execution: Execution) -> None:
        self._execution = execution
        self.calls: list[str] = []

    async def run(self, command: str, **_: Any) -> Execution:
        self.calls.append(command)
        return self._execution


class _AsyncFilesStub:
    def __init__(self) -> None:
        self.writes: list[list[WriteEntry]] = []

    async def write_files(self, entries: list[WriteEntry]) -> None:
        # Copy to keep a stable record even if callers mutate later.
        self.writes.append(list(entries))


class _SyncCommandsStub:
    def __init__(self, execution: Execution) -> None:
        self._execution = execution
        self.calls: list[str] = []

    def run(self, command: str, **_: Any) -> Execution:
        self.calls.append(command)
        return self._execution


class _SyncFilesStub:
    def __init__(self) -> None:
        self.writes: list[list[WriteEntry]] = []

    def write_files(self, entries: list[WriteEntry]) -> None:
        self.writes.append(list(entries))


def _ok() -> Execution:
    return Execution(id="e-1", exit_code=0)


def _bad() -> Execution:
    return Execution(
        id="e-1",
        exit_code=1,
        error=ExecutionError(name="MountError", value="denied", timestamp=0),
        logs=ExecutionLogs(stderr=[OutputMessage(text="stderr text", timestamp=0)]),
    )


def _make_async_sandbox(commands: Any, files: Any) -> Any:
    """Build a minimally-initialised async Sandbox that only wires the two services."""
    from opensandbox.sandbox import Sandbox

    sandbox = Sandbox.__new__(Sandbox)
    sandbox.id = "sbx"
    sandbox._command_service = commands
    sandbox._filesystem_service = files
    return sandbox


def _make_sync_sandbox(commands: Any, files: Any) -> Any:
    from opensandbox.sync.sandbox import SandboxSync

    sandbox = SandboxSync.__new__(SandboxSync)
    sandbox.id = "sbx"
    sandbox._command_service = commands
    sandbox._filesystem_service = files
    return sandbox


# -------- async facade --------


@pytest.mark.asyncio
async def test_async_sandbox_mount_nfs_invokes_commands_run_once() -> None:
    commands = _AsyncCommandsStub(_ok())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    execution = await sandbox.mount(
        NfsMountOptions(endpoint="nas", nas_path="/", mount_point="/mnt/nas")
    )
    assert execution.exit_code == 0
    assert len(commands.calls) == 1
    assert commands.calls[0].startswith("mkdir -p /mnt/nas && ")
    assert "mount -t nfs" in commands.calls[0]
    # NFS does not touch filesystem
    assert files.writes == []


@pytest.mark.asyncio
async def test_async_sandbox_mount_ossfs1_uploads_passwd_file() -> None:
    commands = _AsyncCommandsStub(_ok())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    await sandbox.mount(
        OssfsMountOptions(
            endpoint="e",
            bucket="b",
            mount_point="/mnt/oss",
            access_key_id="AK",
            access_key_secret="SK",
            version=OssfsVersion.OSSFS_1_0,
        )
    )
    assert len(files.writes) == 1
    assert len(files.writes[0]) == 1
    passwd_entry = files.writes[0][0]
    assert passwd_entry.path.startswith("/tmp/opensandbox-ossfspass-")
    assert passwd_entry.data == "b:AK:SK"
    assert passwd_entry.mode == 600
    assert len(commands.calls) == 1
    cmd = commands.calls[0]
    assert "ossfs --version" in cmd
    assert f"-opasswd_file={passwd_entry.path}" in cmd
    assert "AK" not in cmd, f"AK must not leak into shell command, got: {cmd}"
    assert "SK" not in cmd, f"SK must not leak into shell command, got: {cmd}"


@pytest.mark.asyncio
async def test_async_sandbox_mount_ossfs2_uploads_conf_and_env_then_runs_command() -> None:
    commands = _AsyncCommandsStub(_ok())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    await sandbox.mount(
        OssfsMountOptions(
            endpoint="e",
            bucket="b",
            mount_point="/mnt/oss2",
            access_key_id="AK",
            access_key_secret="SK",
            version=OssfsVersion.OSSFS_2_0,
        )
    )
    assert len(files.writes) == 1
    assert len(files.writes[0]) == 2, "ossfs2 must upload both conf and env files"
    by_path = {e.path: e for e in files.writes[0]}
    conf_entry = next(e for p, e in by_path.items() if p.endswith(".conf"))
    env_entry = next(e for p, e in by_path.items() if "-ossfsenv-" in p)
    assert "--oss_bucket=b" in conf_entry.data
    assert conf_entry.mode == 600
    assert "OSS_ACCESS_KEY_ID=AK\n" in env_entry.data
    assert "OSS_ACCESS_KEY_SECRET=SK\n" in env_entry.data
    assert env_entry.mode == 600
    assert len(commands.calls) == 1
    cmd = commands.calls[0]
    assert f". {env_entry.path}" in cmd
    assert f"-c {conf_entry.path}" in cmd
    assert "AK" not in cmd, f"AK must not leak into shell command, got: {cmd}"
    assert "SK" not in cmd, f"SK must not leak into shell command, got: {cmd}"


@pytest.mark.asyncio
async def test_async_sandbox_mount_raises_mount_failed_on_error_execution() -> None:
    commands = _AsyncCommandsStub(_bad())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    with pytest.raises(MountFailedException) as exc:
        await sandbox.mount(
            NfsMountOptions(endpoint="nas", nas_path="/", mount_point="/mnt/nas")
        )
    assert "NAS mount failure" in str(exc.value)
    assert exc.value.execution is commands._execution  # type: ignore[attr-defined]


@pytest.mark.asyncio
async def test_async_sandbox_umount_runs_umount_and_raises_on_failure() -> None:
    commands = _AsyncCommandsStub(_ok())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    await sandbox.umount("/mnt/nas")
    assert commands.calls == ["umount /mnt/nas"]

    commands_bad = _AsyncCommandsStub(_bad())
    sandbox_bad = _make_async_sandbox(commands_bad, files)
    with pytest.raises(MountFailedException):
        await sandbox_bad.umount("/mnt/nas")


@pytest.mark.asyncio
async def test_async_sandbox_mount_rejects_unknown_options_type() -> None:
    commands = _AsyncCommandsStub(_ok())
    files = _AsyncFilesStub()
    sandbox = _make_async_sandbox(commands, files)
    with pytest.raises(TypeError):
        await sandbox.mount(object())  # type: ignore[arg-type]


# -------- sync facade --------


def test_sync_sandbox_mount_nfs_invokes_commands_run_once() -> None:
    commands = _SyncCommandsStub(_ok())
    files = _SyncFilesStub()
    sandbox = _make_sync_sandbox(commands, files)
    execution = sandbox.mount(
        NfsMountOptions(endpoint="nas", nas_path="/", mount_point="/mnt/nas")
    )
    assert execution.exit_code == 0
    assert len(commands.calls) == 1
    assert "mount -t nfs" in commands.calls[0]


def test_sync_sandbox_mount_ossfs2_uploads_conf_and_env_then_runs_command() -> None:
    commands = _SyncCommandsStub(_ok())
    files = _SyncFilesStub()
    sandbox = _make_sync_sandbox(commands, files)
    sandbox.mount(
        OssfsMountOptions(
            endpoint="e",
            bucket="b",
            mount_point="/mnt/oss2",
            access_key_id="AK",
            access_key_secret="SK",
            version=OssfsVersion.OSSFS_2_0,
        )
    )
    assert len(files.writes) == 1
    paths = [e.path for e in files.writes[0]]
    assert any(p.endswith(".conf") for p in paths)
    assert any("-ossfsenv-" in p for p in paths)
    cmd = commands.calls[0]
    assert "AK" not in cmd
    assert "SK" not in cmd


def test_sync_sandbox_umount_raises_on_failure() -> None:
    commands = _SyncCommandsStub(_bad())
    files = _SyncFilesStub()
    sandbox = _make_sync_sandbox(commands, files)
    with pytest.raises(MountFailedException):
        sandbox.umount("/mnt/nas")
