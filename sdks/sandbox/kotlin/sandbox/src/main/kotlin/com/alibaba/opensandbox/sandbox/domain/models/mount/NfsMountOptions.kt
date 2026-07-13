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
 * Options describing an NFS mount to run inside a sandbox with `sandbox.mount(...)`.
 *
 * The SDK does not manage the NAS service — it assembles a `mount -t nfs` shell
 * command and runs it via [com.alibaba.opensandbox.sandbox.domain.services.Commands.run].
 * The sandbox image is expected to already contain the required `mount.nfs` /
 * `nfs-utils` binaries, or [installation] can be provided to install them at mount time.
 *
 * @property endpoint NFS server endpoint (host or IP without the export path).
 * @property nasPath  Export path on the NFS server (for example `/`).
 * @property mountPoint Absolute path inside the sandbox where the export will be mounted.
 *                     The directory is created if it does not exist.
 * @property options Optional NFS mount option string (comma-separated, as accepted by `mount -o`).
 *                   Defaults to a common NFSv3 profile when null or blank.
 * @property installation Optional shell command executed before the mount command,
 *                        typically to install NFS client packages (for example
 *                        `apt-get install -y nfs-common`). Runs on every mount call.
 */
class NfsMountOptions private constructor(
    val endpoint: String,
    val nasPath: String,
    val mountPoint: String,
    val options: String? = null,
    val installation: String? = null,
) {
    companion object {
        /** Default NFSv3 option string applied when [options] is null or blank. */
        const val DEFAULT_NFS_OPTIONS: String =
            "vers=3,nolock,proto=tcp,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport"

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var endpoint: String? = null
        private var nasPath: String? = null
        private var mountPoint: String? = null
        private var options: String? = null
        private var installation: String? = null

        fun endpoint(endpoint: String): Builder = apply { this.endpoint = endpoint }

        fun nasPath(nasPath: String): Builder = apply { this.nasPath = nasPath }

        fun mountPoint(mountPoint: String): Builder = apply { this.mountPoint = mountPoint }

        fun options(options: String?): Builder = apply { this.options = options }

        fun installation(installation: String?): Builder = apply { this.installation = installation }

        fun build(): NfsMountOptions {
            return NfsMountOptions(
                endpoint = requireNotNull(endpoint) { "endpoint must not be null" },
                nasPath = requireNotNull(nasPath) { "nasPath must not be null" },
                mountPoint = requireNotNull(mountPoint) { "mountPoint must not be null" },
                options = options,
                installation = installation,
            )
        }
    }
}
