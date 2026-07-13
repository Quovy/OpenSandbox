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

package com.alibaba.opensandbox.sandbox.mount

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.MountFailedException
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import com.alibaba.opensandbox.sandbox.domain.models.mount.NfsMountOptions
import com.alibaba.opensandbox.sandbox.domain.models.mount.OssfsMountOptions
import java.util.UUID

/**
 * Syntax sugar that mounts an NFS export inside the sandbox by running the
 * appropriate `mount -t nfs` command via [Sandbox.commands].
 *
 * The remote sandbox must already have `mount.nfs` available, or
 * [NfsMountOptions.installation] must install it.
 *
 * @throws InvalidArgumentException if the options are invalid.
 * @throws MountFailedException if the mount command fails (non-zero exit or `error` event).
 */
@JvmName("mountNfs")
fun Sandbox.mount(options: NfsMountOptions): Execution {
    validateNfs(options)
    val optString = options.options?.takeIf { it.isNotBlank() } ?: NfsMountOptions.DEFAULT_NFS_OPTIONS
    val core =
        "mkdir -p ${shQuote(options.mountPoint)} && " +
            "mount -t nfs -o ${shQuote(optString)} " +
            "${shQuote(options.endpoint + ":" + options.nasPath)} ${shQuote(options.mountPoint)}"
    val cmd = prependInstallation(options.installation, core)
    val execution = commands().run(cmd)
    ensureSuccess(execution, "NAS mount failure")
    return execution
}

/**
 * Syntax sugar that mounts an Alibaba Cloud OSS bucket inside the sandbox using
 * `ossfs` (1.x) or `ossfs2` (2.x), selected by [OssfsMountOptions.version].
 *
 * @throws InvalidArgumentException if the options are invalid.
 * @throws MountFailedException if the mount command fails.
 */
@JvmName("mountOssfs")
fun Sandbox.mount(options: OssfsMountOptions): Execution {
    validateOssfs(options)
    return when (options.version ?: OssfsMountOptions.Version.OSSFS_1_0) {
        OssfsMountOptions.Version.OSSFS_1_0 -> mountOssfs1(options)
        OssfsMountOptions.Version.OSSFS_2_0 -> mountOssfs2(options)
    }
}

/**
 * Unmount a previously mounted path inside the sandbox using `umount`.
 *
 * @throws MountFailedException if the umount command fails.
 */
fun Sandbox.umount(mountPoint: String): Execution {
    if (mountPoint.isBlank()) {
        throw InvalidArgumentException(message = "mountPoint must not be blank")
    }
    val execution = commands().run("umount ${shQuote(mountPoint)}")
    ensureSuccess(execution, "umount failure")
    return execution
}

private fun Sandbox.mountOssfs1(options: OssfsMountOptions): Execution {
    val useSts = !options.securityToken.isNullOrBlank()
    // ossfs 1.x password file format:
    //   AK/SK mode : bucket:accessKeyId:accessKeySecret
    //   STS mode   : bucket:accessKeyId:accessKeySecret:securityToken
    val passwd =
        buildString {
            append(options.bucket).append(':')
            append(options.accessKeyId).append(':')
            append(options.accessKeySecret)
            if (useSts) {
                append(':').append(options.securityToken)
            }
        }
    val bucketArg =
        if (!options.bucketDirectory.isNullOrBlank()) {
            "${options.bucket}:/${options.bucketDirectory}"
        } else {
            options.bucket
        }
    val optionFlags =
        options.options.joinToString(separator = "") { " -o${shQuote(it)}" }

    // Upload the password file with 0600 permissions via the filesystem API so
    // the secret material never appears in the mount shell command (which
    // execd logs). Use a unique per-call path under /tmp so concurrent
    // ossfs 1.x mounts do not race on the same file. Always clean it up even
    // if ossfs fails, by preserving the subshell exit code via __rc.
    val passwdPath = "/tmp/opensandbox-ossfspass-${UUID.randomUUID()}"
    files().writeFile(
        WriteEntry.builder()
            .path(passwdPath)
            .data(passwd)
            // Mode is serialized as a JSON number and parsed by execd as an
            // octal string (see components/execd/pkg/web/controller/utils.go);
            // pass the literal decimal 600 to get filesystem mode 0o600.
            .mode(600)
            .build(),
    )

    val quotedPasswdPath = shQuote(passwdPath)
    val core =
        "ossfs --version && " +
            "mkdir -p ${shQuote(options.mountPoint)} && " +
            "( ossfs ${shQuote(bucketArg)} ${shQuote(options.mountPoint)} " +
            "-ourl=${shQuote(options.endpoint)} " +
            "-opasswd_file=$quotedPasswdPath$optionFlags ); " +
            "__rc=\$?; rm -f $quotedPasswdPath; exit \$__rc"

    val cmd = prependInstallation(options.installation, core)
    val execution = commands().run(cmd)
    ensureSuccess(execution, "ossfs1.0 mount failure")
    return execution
}

private fun Sandbox.mountOssfs2(options: OssfsMountOptions): Execution {
    val confBuilder = StringBuilder()
    confBuilder.append("--oss_endpoint=").append(options.endpoint).append('\n')
    confBuilder.append("--oss_bucket=").append(options.bucket).append('\n')
    if (!options.bucketDirectory.isNullOrBlank()) {
        // ossfs2 mounts a bucket root; a subdirectory is expressed as a prefix
        // (trailing slash makes it a directory boundary).
        val prefix = options.bucketDirectory.trimEnd('/') + "/"
        confBuilder.append("--oss_bucket_prefix=").append(prefix).append('\n')
    }
    for (opt in options.options) {
        confBuilder.append("--").append(opt).append('\n')
    }

    val confPath = "/tmp/opensandbox-ossfs-${UUID.randomUUID()}.conf"

    // ossfs 2.x reads credentials from environment variables:
    //   AK/SK mode : OSS_ACCESS_KEY_ID + OSS_ACCESS_KEY_SECRET
    //   STS mode   : additionally OSS_SESSION_TOKEN
    // Upload them via a separate env file so the credentials never appear in
    // the mount shell command (which execd logs).
    val envBuilder = StringBuilder()
    envBuilder.append("OSS_ACCESS_KEY_ID=").append(options.accessKeyId).append('\n')
    envBuilder.append("OSS_ACCESS_KEY_SECRET=").append(options.accessKeySecret).append('\n')
    if (!options.securityToken.isNullOrBlank()) {
        envBuilder.append("OSS_SESSION_TOKEN=").append(options.securityToken).append('\n')
    }
    val envPath = "/tmp/opensandbox-ossfsenv-${UUID.randomUUID()}"

    files().write(
        listOf(
            WriteEntry.builder()
                .path(confPath)
                .data(confBuilder.toString())
                // Mode is serialized as a JSON number and parsed by execd as
                // an octal string (see
                // components/execd/pkg/web/controller/utils.go); pass the
                // literal decimal 600 to get filesystem mode 0o600.
                .mode(600)
                .build(),
            WriteEntry.builder()
                .path(envPath)
                .data(envBuilder.toString())
                .mode(600)
                .build(),
        ),
    )

    // Always remove the ossfs2 conf and env files after the mount attempt,
    // even on failure. The subshell preserves the ossfs2 exit code via __rc.
    // `set -a` marks subsequently defined vars for export; sourcing the env
    // file makes OSS_ACCESS_KEY_ID / SECRET / SESSION_TOKEN available to
    // ossfs2 without emitting them into the shell command text.
    val quotedConfPath = shQuote(confPath)
    val quotedEnvPath = shQuote(envPath)
    val core =
        "ossfs2 --version && " +
            "mkdir -p ${shQuote(options.mountPoint)} && " +
            "set -a && . $quotedEnvPath && set +a && " +
            "( ossfs2 mount ${shQuote(options.mountPoint)} -c $quotedConfPath ); " +
            "__rc=\$?; rm -f $quotedConfPath $quotedEnvPath; exit \$__rc"

    val cmd = prependInstallation(options.installation, core)
    val execution = commands().run(cmd)
    ensureSuccess(execution, "ossfs2.0 mount failure")
    return execution
}

private fun validateNfs(options: NfsMountOptions) {
    if (options.endpoint.isBlank()) throw InvalidArgumentException(message = "endpoint must not be blank")
    if (options.mountPoint.isBlank()) throw InvalidArgumentException(message = "mountPoint must not be blank")
    if (options.nasPath.isBlank()) throw InvalidArgumentException(message = "nasPath must not be blank")
}

private fun validateOssfs(options: OssfsMountOptions) {
    if (options.endpoint.isBlank()) throw InvalidArgumentException(message = "endpoint must not be blank")
    if (options.bucket.isBlank()) throw InvalidArgumentException(message = "bucket must not be blank")
    if (options.mountPoint.isBlank()) throw InvalidArgumentException(message = "mountPoint must not be blank")
    if (options.accessKeyId.isBlank()) throw InvalidArgumentException(message = "accessKeyId must not be blank")
    if (options.accessKeySecret.isBlank()) throw InvalidArgumentException(message = "accessKeySecret must not be blank")
}

private fun ensureSuccess(
    execution: Execution,
    failurePrefix: String,
) {
    val error = execution.error
    val exitCode = execution.exitCode
    val failed = error != null || (exitCode != null && exitCode != 0)
    if (!failed) return
    val stderr = execution.logs.stderr.joinToString("\n") { it.text }
    val detail =
        buildString {
            if (error != null) {
                append('[').append(error.name).append("] ").append(error.value)
            }
            if (stderr.isNotEmpty()) {
                if (isNotEmpty()) append(" | ")
                append("stderr=").append(stderr)
            }
        }
    throw MountFailedException(
        message = if (detail.isNotEmpty()) "$failurePrefix: $detail" else failurePrefix,
        execution = execution,
    )
}

private fun prependInstallation(
    installation: String?,
    core: String,
): String {
    return if (!installation.isNullOrBlank()) "$installation && $core" else core
}

/**
 * Quote a value for POSIX shell single-quoted context.
 *
 * `'` inside the value is escaped as `'\''`. The result is always safe to embed
 * as one argument to a `sh -c` string.
 */
internal fun shQuote(value: String): String {
    val escaped = value.replace("'", "'\\''")
    return "'$escaped'"
}
