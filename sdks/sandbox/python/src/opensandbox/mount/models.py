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
"""Options models for NAS and OSS mount syntax sugar."""

from __future__ import annotations

from enum import Enum

from pydantic import BaseModel, ConfigDict, Field

#: Default NFSv3 option string applied when ``NfsMountOptions.options`` is None or blank.
DEFAULT_NFS_OPTIONS: str = (
    "vers=3,nolock,proto=tcp,rsize=1048576,wsize=1048576,"
    "hard,timeo=600,retrans=2,noresvport"
)


class NfsMountOptions(BaseModel):
    """
    Options describing an NFS mount executed inside a sandbox via ``sandbox.mount(...)``.

    The SDK does not manage the NAS service. It assembles a ``mount -t nfs`` shell
    command and runs it through ``sandbox.commands.run``. The sandbox image must
    already contain the ``mount.nfs`` binary, or ``installation`` can install it.
    """

    endpoint: str = Field(description="NFS server endpoint (host or IP, no export path).")
    nas_path: str = Field(description="Export path on the NFS server (for example '/').")
    mount_point: str = Field(
        description="Absolute path inside the sandbox where the export will be mounted."
    )
    options: str | None = Field(
        default=None,
        description="Comma-separated NFS mount options. Defaults to a common NFSv3 profile.",
    )
    installation: str | None = Field(
        default=None,
        description=(
            "Optional shell command executed before the mount command "
            "(for example to install nfs-utils)."
        ),
    )

    model_config = ConfigDict(populate_by_name=True)


class OssfsVersion(str, Enum):
    """ossfs binary major version."""

    OSSFS_1_0 = "1.0"
    OSSFS_2_0 = "2.0"


class OssfsMountOptions(BaseModel):
    """
    Options describing an Alibaba Cloud OSS mount executed inside a sandbox with
    either ``ossfs`` (1.x) or ``ossfs2`` (2.x).

    When ``version`` is None the SDK defaults to :attr:`OssfsVersion.OSSFS_1_0`.
    ``bucket_directory`` is supported by both versions:

    - ossfs 1.x: mounts ``bucket:/dir``.
    - ossfs 2.x: written to the configuration file as ``--oss_bucket_prefix=<dir>/``.
    """

    endpoint: str = Field(description="OSS endpoint URL.")
    bucket: str = Field(description="OSS bucket name.")
    mount_point: str = Field(
        description="Absolute path inside the sandbox where the bucket is mounted."
    )
    access_key_id: str = Field(description="Alibaba Cloud access key id.")
    access_key_secret: str = Field(description="Alibaba Cloud access key secret.")
    security_token: str | None = Field(
        default=None,
        description=(
            "Optional STS security token. When set, ossfs 1.x appends it to the "
            "password file; ossfs 2.x exports it as OSS_SESSION_TOKEN."
        ),
    )
    version: OssfsVersion | None = Field(
        default=None,
        description="ossfs major version. Defaults to OSSFS_1_0 when omitted.",
    )
    bucket_directory: str | None = Field(
        default=None,
        description="Optional subdirectory inside the bucket to mount.",
    )
    options: list[str] = Field(
        default_factory=list,
        description=(
            "Additional ossfs options. Each entry is the raw option value without "
            "the leading '-o' (ossfs 1.x) or '--' (ossfs 2.x) prefix."
        ),
    )
    installation: str | None = Field(
        default=None,
        description="Optional shell command executed before the mount command.",
    )

    model_config = ConfigDict(populate_by_name=True)
