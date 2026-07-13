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

package com.alibaba.opensandbox.sandbox.domain.models.mount

/**
 * Options describing an Alibaba Cloud OSS mount (ossfs 1.x or ossfs 2.x) to run
 * inside a sandbox with `sandbox.mount(...)`.
 *
 * The SDK assembles the appropriate `ossfs` / `ossfs2` shell command and runs it
 * via [com.alibaba.opensandbox.sandbox.domain.services.Commands.run]. The
 * sandbox image must contain the corresponding `ossfs` binary, or
 * [installation] can be used to install it at mount time.
 *
 * ## Version selection
 * When [version] is null, [Version.OSSFS_1_0] is used. Set [Version.OSSFS_2_0]
 * to use ossfs2, which reads credentials from environment variables and reads
 * options from a configuration file written by the SDK.
 *
 * @property endpoint OSS endpoint URL (for example `https://oss-cn-hangzhou.aliyuncs.com`).
 * @property bucket OSS bucket name.
 * @property mountPoint Absolute path inside the sandbox where the bucket will be mounted.
 * @property accessKeyId Alibaba Cloud access key id.
 * @property accessKeySecret Alibaba Cloud access key secret.
 * @property securityToken Optional STS security token. When set, the credentials
 *                         become STS-mode. ossfs 1.x appends it to the password
 *                         file; ossfs 2.x exports it as `OSS_SESSION_TOKEN`.
 * @property version Optional ossfs major version, defaults to [Version.OSSFS_1_0].
 * @property bucketDirectory Optional subdirectory inside the bucket. ossfs 1.x
 *                           mounts `bucket:/dir`; ossfs 2.x sets
 *                           `--oss_bucket_prefix=<dir>/` in the configuration file.
 * @property options Additional ossfs options passed on the command line
 *                   (ossfs 1.x) or written to the configuration file (ossfs 2.x).
 *                   Each entry is a raw option value without the leading `-o` /
 *                   `--` prefix (for example `use_cache=/tmp/ossfs`).
 * @property installation Optional shell command executed before the mount command,
 *                        typically to install `ossfs` / `ossfs2` packages.
 */
class OssfsMountOptions private constructor(
    val endpoint: String,
    val bucket: String,
    val mountPoint: String,
    val accessKeyId: String,
    val accessKeySecret: String,
    val securityToken: String? = null,
    val version: Version? = null,
    val bucketDirectory: String? = null,
    val options: List<String> = emptyList(),
    val installation: String? = null,
) {
    /** ossfs binary major version. */
    enum class Version(val wireValue: String) {
        OSSFS_1_0("1.0"),
        OSSFS_2_0("2.0"),
        ;

        companion object {
            /** Parse a wire value ("1.0" / "2.0"). Returns null for null input. */
            @JvmStatic
            fun fromWireValue(value: String?): Version? =
                when (value) {
                    null -> null
                    "1.0" -> OSSFS_1_0
                    "2.0" -> OSSFS_2_0
                    else -> throw IllegalArgumentException("Unsupported ossfs version: $value")
                }
        }
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var endpoint: String? = null
        private var bucket: String? = null
        private var mountPoint: String? = null
        private var accessKeyId: String? = null
        private var accessKeySecret: String? = null
        private var securityToken: String? = null
        private var version: Version? = null
        private var bucketDirectory: String? = null
        private var options: MutableList<String> = mutableListOf()
        private var installation: String? = null

        fun endpoint(endpoint: String): Builder = apply { this.endpoint = endpoint }

        fun bucket(bucket: String): Builder = apply { this.bucket = bucket }

        fun mountPoint(mountPoint: String): Builder = apply { this.mountPoint = mountPoint }

        fun accessKeyId(accessKeyId: String): Builder = apply { this.accessKeyId = accessKeyId }

        fun accessKeySecret(accessKeySecret: String): Builder = apply { this.accessKeySecret = accessKeySecret }

        fun securityToken(securityToken: String?): Builder = apply { this.securityToken = securityToken }

        fun version(version: Version?): Builder = apply { this.version = version }

        /** Convenience overload accepting the wire value string ("1.0" / "2.0"). */
        fun version(wireValue: String?): Builder = apply { this.version = Version.fromWireValue(wireValue) }

        fun bucketDirectory(bucketDirectory: String?): Builder = apply { this.bucketDirectory = bucketDirectory }

        fun option(option: String): Builder = apply { this.options.add(option) }

        fun options(options: List<String>): Builder =
            apply {
                this.options.clear()
                this.options.addAll(options)
            }

        fun installation(installation: String?): Builder = apply { this.installation = installation }

        fun build(): OssfsMountOptions {
            return OssfsMountOptions(
                endpoint = requireNotNull(endpoint) { "endpoint must not be null" },
                bucket = requireNotNull(bucket) { "bucket must not be null" },
                mountPoint = requireNotNull(mountPoint) { "mountPoint must not be null" },
                accessKeyId = requireNotNull(accessKeyId) { "accessKeyId must not be null" },
                accessKeySecret = requireNotNull(accessKeySecret) { "accessKeySecret must not be null" },
                securityToken = securityToken,
                version = version,
                bucketDirectory = bucketDirectory,
                options = options.toList(),
                installation = installation,
            )
        }
    }
}
