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

/**
 * Default NFSv3 option string applied when {@link NfsMountOptions.options} is undefined or blank.
 */
export const DEFAULT_NFS_OPTIONS =
  "vers=3,nolock,proto=tcp,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport";

/**
 * Options describing an NFS mount executed inside a sandbox via `sandbox.mount(...)`.
 *
 * The SDK does not manage the NAS service. It assembles a `mount -t nfs` shell
 * command and runs it via `sandbox.commands.run`. The sandbox image must
 * already contain the `mount.nfs` binary, or {@link installation} can install it.
 */
export interface NfsMountOptions {
  /** NFS server endpoint (host or IP, without the export path). */
  endpoint: string;
  /** Export path on the NFS server (for example `/`). */
  nasPath: string;
  /** Absolute path inside the sandbox where the export will be mounted. */
  mountPoint: string;
  /**
   * Comma-separated NFS mount options passed to `mount -o`.
   * Defaults to {@link DEFAULT_NFS_OPTIONS} when omitted or blank.
   */
  options?: string;
  /**
   * Optional shell command executed before the mount command, typically to install
   * NFS client packages (for example `apt-get install -y nfs-common`).
   */
  installation?: string;
}

/** ossfs binary major version. */
export type OssfsVersion = "1.0" | "2.0";

/**
 * Options describing an Alibaba Cloud OSS mount executed inside a sandbox with
 * either `ossfs` (1.x) or `ossfs2` (2.x).
 *
 * `bucketDirectory` is supported by both versions:
 * - ossfs 1.x: mounts `bucket:/dir`.
 * - ossfs 2.x: written to the configuration file as `--oss_bucket_prefix=<dir>/`.
 */
export interface OssfsMountOptions {
  /** OSS endpoint URL (e.g. `https://oss-cn-hangzhou.aliyuncs.com`). */
  endpoint: string;
  /** OSS bucket name. */
  bucket: string;
  /** Absolute path inside the sandbox where the bucket will be mounted. */
  mountPoint: string;
  /** Alibaba Cloud access key id. */
  accessKeyId: string;
  /** Alibaba Cloud access key secret. */
  accessKeySecret: string;
  /**
   * Optional STS security token.
   *
   * When set, ossfs 1.x appends it to the password file and ossfs 2.x exports
   * it as `OSS_SESSION_TOKEN`.
   */
  securityToken?: string;
  /** ossfs major version. Defaults to `"1.0"` when omitted. */
  version?: OssfsVersion;
  /** Optional subdirectory inside the bucket to mount. */
  bucketDirectory?: string;
  /**
   * Additional ossfs options. Each entry is the raw option value without the
   * leading `-o` (ossfs 1.x) or `--` (ossfs 2.x) prefix, for example
   * `"use_cache=/tmp/ossfs"`.
   */
  options?: string[];
  /** Optional shell command executed before the mount command. */
  installation?: string;
}

/**
 * Discriminates between {@link NfsMountOptions} and {@link OssfsMountOptions}
 * without leaking a wire-format tag.
 */
export function isNfsMountOptions(
  value: NfsMountOptions | OssfsMountOptions,
): value is NfsMountOptions {
  return typeof (value as NfsMountOptions).nasPath === "string";
}

export function isOssfsMountOptions(
  value: NfsMountOptions | OssfsMountOptions,
): value is OssfsMountOptions {
  return typeof (value as OssfsMountOptions).accessKeyId === "string";
}
