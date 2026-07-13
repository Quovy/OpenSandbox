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
"""
Internal helpers that translate :mod:`opensandbox.mount.models` requests into
shell commands and validate execution results.

These helpers are shared by the async and sync mount implementations so both
SDK entry points build identical commands.
"""

from __future__ import annotations

import shlex
import uuid
from dataclasses import dataclass

from opensandbox.exceptions import InvalidArgumentException, MountFailedException
from opensandbox.models.execd import Execution
from opensandbox.models.filesystem import WriteEntry
from opensandbox.mount.models import (
    DEFAULT_NFS_OPTIONS,
    NfsMountOptions,
    OssfsMountOptions,
    OssfsVersion,
)

_OSSFS1_PASSWD_PREFIX = "/tmp/opensandbox-ossfspass-"
"""Prefix for the per-call ossfs 1.x password file path under /tmp."""


@dataclass(frozen=True)
class Ossfs1Plan:
    """
    Plan for an ossfs 1.x mount.

    The password file is uploaded through the filesystem API (mode 0600); the
    ``command`` string references it via ``-opasswd_file`` but never contains
    the credentials themselves, so execd's command log cannot leak AK/SK.
    """

    passwd_path: str
    passwd_content: str
    command: str


@dataclass(frozen=True)
class Ossfs2Plan:
    """
    Plan for an ossfs 2.x mount.

    Both the ossfs2 conf file (endpoint / bucket / options) and a separate env
    file that carries the AK/SK[/session token] are uploaded through the
    filesystem API (mode 0600). The mount command sources the env file, so no
    credential ever appears in the command text (which execd logs).
    """

    conf_path: str
    conf_content: str
    env_path: str
    env_content: str
    command: str


def validate_nfs(options: NfsMountOptions) -> None:
    """Validate NFS options before assembling the mount command."""
    if not options.endpoint or not options.endpoint.strip():
        raise InvalidArgumentException("endpoint must not be blank")
    if not options.mount_point or not options.mount_point.strip():
        raise InvalidArgumentException("mount_point must not be blank")
    if not options.nas_path or not options.nas_path.strip():
        raise InvalidArgumentException("nas_path must not be blank")


def validate_ossfs(options: OssfsMountOptions) -> None:
    """Validate OSS options before assembling the mount command."""
    if not options.endpoint or not options.endpoint.strip():
        raise InvalidArgumentException("endpoint must not be blank")
    if not options.bucket or not options.bucket.strip():
        raise InvalidArgumentException("bucket must not be blank")
    if not options.mount_point or not options.mount_point.strip():
        raise InvalidArgumentException("mount_point must not be blank")
    if not options.access_key_id or not options.access_key_id.strip():
        raise InvalidArgumentException("access_key_id must not be blank")
    if not options.access_key_secret or not options.access_key_secret.strip():
        raise InvalidArgumentException("access_key_secret must not be blank")


def build_nfs_command(options: NfsMountOptions) -> str:
    """
    Assemble the shell command that mounts an NFS export.

    Layout:
        [installation && ] mkdir -p <mnt> && mount -t nfs -o <opts> <server>:<path> <mnt>
    """
    opt_string = (
        options.options if options.options and options.options.strip() else DEFAULT_NFS_OPTIONS
    )
    source = f"{options.endpoint}:{options.nas_path}"
    core = (
        f"mkdir -p {shlex.quote(options.mount_point)} && "
        f"mount -t nfs -o {shlex.quote(opt_string)} "
        f"{shlex.quote(source)} {shlex.quote(options.mount_point)}"
    )
    return _prepend_installation(options.installation, core)


def build_ossfs1_plan(options: OssfsMountOptions) -> Ossfs1Plan:
    """
    Build the ossfs 1.x mount plan.

    The password file (``bucket:AK:SK[:token]``) is delivered as an
    :class:`Ossfs1Plan.passwd_content` string, which the caller must upload
    via the filesystem API (mode 0600) *before* running ``plan.command``.
    Because the credentials only travel through the filesystem write, the
    generated shell command references the path via ``-opasswd_file`` but
    never embeds AK/SK, so execd's command log cannot capture them.

    The path is unique per call so concurrent ossfs 1.x mounts in the same
    sandbox never race on the same file, and it is removed after the mount
    attempt regardless of the ossfs exit code.
    """
    passwd = f"{options.bucket}:{options.access_key_id}:{options.access_key_secret}"
    if options.security_token and options.security_token.strip():
        # STS mode: bucket:accessKeyId:accessKeySecret:securityToken
        passwd = f"{passwd}:{options.security_token}"

    bucket_arg = options.bucket
    if options.bucket_directory and options.bucket_directory.strip():
        bucket_arg = f"{options.bucket}:/{options.bucket_directory}"

    option_flags = "".join(f" -o{shlex.quote(opt)}" for opt in options.options)

    passwd_path = f"{_OSSFS1_PASSWD_PREFIX}{uuid.uuid4()}"
    quoted_passwd_path = shlex.quote(passwd_path)
    core = (
        "ossfs --version && "
        f"mkdir -p {shlex.quote(options.mount_point)} && "
        f"( ossfs {shlex.quote(bucket_arg)} {shlex.quote(options.mount_point)} "
        f"-ourl={shlex.quote(options.endpoint)} "
        f"-opasswd_file={quoted_passwd_path}{option_flags} ); "
        f"__rc=$?; rm -f {quoted_passwd_path}; exit $__rc"
    )
    command = _prepend_installation(options.installation, core)
    return Ossfs1Plan(
        passwd_path=passwd_path,
        passwd_content=passwd,
        command=command,
    )


def build_ossfs1_passwd_entry(plan: Ossfs1Plan) -> WriteEntry:
    """Convert an :class:`Ossfs1Plan` into a :class:`WriteEntry` for upload."""
    return WriteEntry(path=plan.passwd_path, data=plan.passwd_content, mode=600)


def build_ossfs2_plan(options: OssfsMountOptions) -> Ossfs2Plan:
    """
    Build the ossfs 2.x mount plan.

    Both the ossfs2 conf file (endpoint / bucket / options) and a separate
    env file that carries ``OSS_ACCESS_KEY_ID`` / ``OSS_ACCESS_KEY_SECRET``
    ``[/ OSS_SESSION_TOKEN]`` are returned as ``WriteEntry``-friendly
    string content; the caller must upload both (mode 0600) *before* running
    ``plan.command``. The generated shell command sources the env file with
    ``set -a && . <env>``, so no credential ever appears in the command
    text (which execd logs). Both files are removed after the mount attempt
    regardless of the ossfs2 exit code.
    """
    lines = [
        f"--oss_endpoint={options.endpoint}",
        f"--oss_bucket={options.bucket}",
    ]
    if options.bucket_directory and options.bucket_directory.strip():
        prefix = options.bucket_directory.rstrip("/") + "/"
        lines.append(f"--oss_bucket_prefix={prefix}")
    for opt in options.options:
        lines.append(f"--{opt}")
    conf_content = "\n".join(lines) + "\n"

    env_lines = [
        f"OSS_ACCESS_KEY_ID={options.access_key_id}",
        f"OSS_ACCESS_KEY_SECRET={options.access_key_secret}",
    ]
    if options.security_token and options.security_token.strip():
        env_lines.append(f"OSS_SESSION_TOKEN={options.security_token}")
    env_content = "\n".join(env_lines) + "\n"

    conf_path = f"/tmp/opensandbox-ossfs-{uuid.uuid4()}.conf"
    env_path = f"/tmp/opensandbox-ossfsenv-{uuid.uuid4()}"
    quoted_conf_path = shlex.quote(conf_path)
    quoted_env_path = shlex.quote(env_path)

    # `set -a` marks subsequently sourced/assigned variables for export.
    # Sourcing the env file makes OSS_ACCESS_KEY_ID / SECRET / SESSION_TOKEN
    # available to ossfs2 without writing them into the shell command text.
    # Both files are removed after the mount attempt via a __rc-preserving
    # cleanup, mirroring the server-side finally block.
    core = (
        "ossfs2 --version && "
        f"mkdir -p {shlex.quote(options.mount_point)} && "
        f"set -a && . {quoted_env_path} && set +a && "
        f"( ossfs2 mount {shlex.quote(options.mount_point)} -c {quoted_conf_path} ); "
        f"__rc=$?; rm -f {quoted_conf_path} {quoted_env_path}; exit $__rc"
    )
    command = _prepend_installation(options.installation, core)
    return Ossfs2Plan(
        conf_path=conf_path,
        conf_content=conf_content,
        env_path=env_path,
        env_content=env_content,
        command=command,
    )


def build_ossfs2_entries(plan: Ossfs2Plan) -> list[WriteEntry]:
    """Convert an :class:`Ossfs2Plan` into the :class:`WriteEntry` list for upload."""
    return [
        WriteEntry(path=plan.conf_path, data=plan.conf_content, mode=600),
        WriteEntry(path=plan.env_path, data=plan.env_content, mode=600),
    ]


def build_umount_command(mount_point: str) -> str:
    """Assemble the shell command that unmounts a path."""
    if not mount_point or not mount_point.strip():
        raise InvalidArgumentException("mount_point must not be blank")
    return f"umount {shlex.quote(mount_point)}"


def select_ossfs_version(options: OssfsMountOptions) -> OssfsVersion:
    """Return the effective ossfs version, defaulting to ossfs 1.0."""
    return options.version or OssfsVersion.OSSFS_1_0


def ensure_success(execution: Execution, failure_prefix: str) -> None:
    """Raise :class:`MountFailedException` if the execution reported an error."""
    error = execution.error
    exit_code = execution.exit_code
    failed = error is not None or (exit_code is not None and exit_code != 0)
    if not failed:
        return

    stderr_text = "\n".join(msg.text for msg in execution.logs.stderr)
    parts: list[str] = []
    if error is not None:
        parts.append(f"[{error.name}] {error.value}")
    if stderr_text:
        parts.append(f"stderr={stderr_text}")
    detail = " | ".join(parts)
    message = f"{failure_prefix}: {detail}" if detail else failure_prefix
    raise MountFailedException(message=message, execution=execution)


def _prepend_installation(installation: str | None, core: str) -> str:
    if installation and installation.strip():
        return f"{installation} && {core}"
    return core
