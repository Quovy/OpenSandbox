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

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.MountFailedException
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionError
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.OutputMessage
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import com.alibaba.opensandbox.sandbox.domain.models.mount.NfsMountOptions
import com.alibaba.opensandbox.sandbox.domain.models.mount.OssfsMountOptions
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.domain.services.CredentialVault
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.Health
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import io.mockk.Runs
import io.mockk.clearMocks
import io.mockk.every
import io.mockk.impl.annotations.MockK
import io.mockk.junit5.MockKExtension
import io.mockk.just
import io.mockk.mockk
import io.mockk.slot
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.extension.ExtendWith

@ExtendWith(MockKExtension::class)
class MountExtensionsTest {
    @MockK
    lateinit var sandboxService: Sandboxes

    @MockK
    lateinit var fileSystemService: Filesystem

    @MockK
    lateinit var commandService: Commands

    @MockK
    lateinit var healthService: Health

    @MockK
    lateinit var metricsService: Metrics

    @MockK
    lateinit var egressService: Egress

    @MockK
    lateinit var credentialVaultService: CredentialVault

    @MockK
    lateinit var diagnosticsService: Diagnostics

    @MockK
    lateinit var httpClientProvider: HttpClientProvider

    private lateinit var sandbox: Sandbox

    @BeforeEach
    fun setUp() {
        every { httpClientProvider.config } returns
            ConnectionConfig.builder()
                .domain("localhost:8080")
                .useServerProxy(false)
                .build()

        sandbox =
            Sandbox(
                id = "sandbox-id",
                sandboxService = sandboxService,
                fileSystemService = fileSystemService,
                commandService = commandService,
                healthService = healthService,
                metricsService = metricsService,
                egressService = egressService,
                credentialVaultService = credentialVaultService,
                isolatedService = mockk(),
                customHealthCheck = null,
                httpClientProvider = httpClientProvider,
                diagnosticsService = diagnosticsService,
            )
    }

    private fun successExecution(): Execution {
        return Execution(id = "e-1", exitCode = 0)
    }

    private fun failedExecution(
        name: String = "Error",
        value: String = "boom",
    ): Execution {
        val exec = Execution(id = "e-1", exitCode = 1)
        exec.error = ExecutionError(name = name, value = value, timestamp = 0L)
        exec.logs.addStderr(OutputMessage(text = "some stderr", timestamp = 0L))
        return exec
    }

    // -------- NFS --------

    @Test
    fun `nfs mount builds mount -t nfs command with default options`() {
        val cmdSlot = slot<String>()
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        val execution =
            sandbox.mount(
                NfsMountOptions.builder()
                    .endpoint("nas-server.example.com")
                    .nasPath("/share")
                    .mountPoint("/mnt/nas")
                    .build(),
            )

        assertNotNull(execution.id)
        val cmd = cmdSlot.captured
        assertTrue(cmd.contains("mkdir -p '/mnt/nas'"), "should mkdir mount point, got: $cmd")
        assertTrue(cmd.contains("mount -t nfs -o "), "should invoke mount -t nfs, got: $cmd")
        assertTrue(cmd.contains(NfsMountOptions.DEFAULT_NFS_OPTIONS), "should use default nfs opts, got: $cmd")
        assertTrue(cmd.contains("'nas-server.example.com:/share' '/mnt/nas'"), "should pass server:path mountPoint, got: $cmd")
        verify(exactly = 1) { commandService.run(any<String>()) }
    }

    @Test
    fun `nfs mount honors custom options and prepends installation`() {
        val cmdSlot = slot<String>()
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        sandbox.mount(
            NfsMountOptions.builder()
                .endpoint("nas")
                .nasPath("/")
                .mountPoint("/mnt/x")
                .options("vers=4,proto=tcp")
                .installation("apt-get install -y nfs-common")
                .build(),
        )

        val cmd = cmdSlot.captured
        assertTrue(cmd.startsWith("apt-get install -y nfs-common && "), "installation should prefix, got: $cmd")
        assertTrue(cmd.contains("'vers=4,proto=tcp'"), "custom nfs opts should be present, got: $cmd")
    }

    @Test
    fun `nfs mount rejects blank endpoint`() {
        val ex =
            assertThrows(InvalidArgumentException::class.java) {
                sandbox.mount(
                    NfsMountOptions.builder()
                        .endpoint("   ")
                        .nasPath("/")
                        .mountPoint("/mnt/x")
                        .build(),
                )
            }
        assertTrue(ex.message!!.contains("endpoint"))
    }

    @Test
    fun `nfs mount raises MountFailedException on error execution`() {
        val bad = failedExecution(name = "MountError", value = "mount.nfs: not permitted")
        every { commandService.run(any<String>()) } returns bad

        val ex =
            assertThrows(MountFailedException::class.java) {
                sandbox.mount(
                    NfsMountOptions.builder()
                        .endpoint("nas")
                        .nasPath("/")
                        .mountPoint("/mnt/x")
                        .build(),
                )
            }
        assertSame(bad, ex.execution)
        assertTrue(ex.message!!.contains("NAS mount failure"))
        assertTrue(ex.message!!.contains("MountError"))
        assertTrue(ex.message!!.contains("stderr=some stderr"))
    }

    // -------- OSS ossfs 1.x --------

    @Test
    fun `ossfs1 mount uploads passwd file with mode 600 and cleans up`() {
        val entrySlot = slot<WriteEntry>()
        val cmdSlot = slot<String>()
        every { fileSystemService.writeFile(capture(entrySlot)) } just Runs
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        sandbox.mount(
            OssfsMountOptions.builder()
                .endpoint("https://oss-cn-hangzhou.aliyuncs.com")
                .bucket("my-bucket")
                .mountPoint("/mnt/oss")
                .accessKeyId("AK")
                .accessKeySecret("SK")
                .build(),
        )

        // passwd file is uploaded via the filesystem API (mode 600), never
        // embedded in the shell command.
        val entry = entrySlot.captured
        assertTrue(
            entry.path.matches(Regex("/tmp/opensandbox-ossfspass-[0-9a-f-]{36}")),
            "passwd file must be under /tmp with a unique suffix, got: ${entry.path}",
        )
        assertEquals(600, entry.mode)
        assertEquals("my-bucket:AK:SK", entry.data as String)

        val cmd = cmdSlot.captured
        val passwdPath = "'${entry.path}'"
        assertTrue(cmd.contains("ossfs 'my-bucket' '/mnt/oss'"))
        assertTrue(cmd.contains("-ourl='https://oss-cn-hangzhou.aliyuncs.com'"))
        assertTrue(cmd.contains("-opasswd_file=$passwdPath"))
        assertTrue(
            cmd.contains("__rc=\$?; rm -f $passwdPath; exit \$__rc"),
            "must always clean up passwd file regardless of ossfs exit code, got: $cmd",
        )
        assertCredentialsAbsentFromCommand(cmd)
    }

    @Test
    fun `ossfs1 mount uses a distinct passwd file per call`() {
        val entries = mutableListOf<WriteEntry>()
        every { fileSystemService.writeFile(capture(entries)) } just Runs
        every { commandService.run(any<String>()) } returns successExecution()

        repeat(2) {
            sandbox.mount(
                OssfsMountOptions.builder()
                    .endpoint("https://oss.example.com")
                    .bucket("b")
                    .mountPoint("/mnt/oss")
                    .accessKeyId("AK")
                    .accessKeySecret("SK")
                    .build(),
            )
        }
        val paths = entries.map { it.path }
        assertEquals(2, paths.size)
        assertEquals(2, paths.toSet().size, "concurrent-safe ossfs1 mounts must not reuse the same passwd file, got: $paths")
    }

    @Test
    fun `ossfs1 mount supports STS security token and options and bucketDirectory`() {
        val entrySlot = slot<WriteEntry>()
        val cmdSlot = slot<String>()
        every { fileSystemService.writeFile(capture(entrySlot)) } just Runs
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        sandbox.mount(
            OssfsMountOptions.builder()
                .endpoint("https://oss-cn-hangzhou.aliyuncs.com")
                .bucket("b")
                .bucketDirectory("subdir")
                .mountPoint("/mnt/oss")
                .accessKeyId("AK")
                .accessKeySecret("SK")
                .securityToken("TOKEN")
                .option("use_cache=/tmp/ossfs")
                .option("allow_other")
                .version(OssfsMountOptions.Version.OSSFS_1_0)
                .build(),
        )

        // passwd file content is uploaded, not embedded in the command.
        assertEquals("b:AK:SK:TOKEN", entrySlot.captured.data as String)
        val cmd = cmdSlot.captured
        assertTrue(cmd.contains("ossfs 'b:/subdir' '/mnt/oss'"), "bucket:/dir syntax should be used, got: $cmd")
        assertTrue(cmd.contains("-o'use_cache=/tmp/ossfs'"))
        assertTrue(cmd.contains("-o'allow_other'"))
        assertCredentialsAbsentFromCommand(cmd)
    }

    // -------- OSS ossfs 2.x --------

    @Test
    fun `ossfs2 mount writes conf and env files and cleans up both`() {
        val entriesSlot = slot<List<WriteEntry>>()
        val cmdSlot = slot<String>()
        every { fileSystemService.write(capture(entriesSlot)) } just Runs
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        sandbox.mount(
            OssfsMountOptions.builder()
                .endpoint("https://oss-cn-hangzhou.aliyuncs.com")
                .bucket("b")
                .mountPoint("/mnt/oss2")
                .accessKeyId("AK")
                .accessKeySecret("SK")
                .option("cache_dir=/tmp/ossfs2")
                .version(OssfsMountOptions.Version.OSSFS_2_0)
                .build(),
        )

        val entries = entriesSlot.captured
        assertEquals(2, entries.size, "ossfs2 should upload both a conf and an env file, got: $entries")
        val conf =
            entries.singleOrNull { it.path.startsWith("/tmp/opensandbox-ossfs-") }
                ?: error("missing conf entry: $entries")
        val env =
            entries.singleOrNull { it.path.startsWith("/tmp/opensandbox-ossfsenv-") }
                ?: error("missing env entry: $entries")

        assertTrue(conf.path.endsWith(".conf"))
        assertEquals(600, conf.mode)
        val confBody = conf.data as String
        assertTrue(confBody.contains("--oss_endpoint=https://oss-cn-hangzhou.aliyuncs.com\n"))
        assertTrue(confBody.contains("--oss_bucket=b\n"))
        assertTrue(confBody.contains("--cache_dir=/tmp/ossfs2\n"))

        assertEquals(600, env.mode)
        val envBody = env.data as String
        assertTrue(envBody.contains("OSS_ACCESS_KEY_ID=AK\n"), "env file must define OSS_ACCESS_KEY_ID, got: $envBody")
        assertTrue(envBody.contains("OSS_ACCESS_KEY_SECRET=SK\n"), "env file must define OSS_ACCESS_KEY_SECRET, got: $envBody")

        val cmd = cmdSlot.captured
        assertTrue(cmd.contains("ossfs2 --version"))
        assertTrue(cmd.contains("set -a && . '${env.path}' && set +a"))
        assertTrue(cmd.contains("( ossfs2 mount '/mnt/oss2' -c '${conf.path}' )"))
        assertTrue(
            cmd.contains("__rc=\$?; rm -f '${conf.path}' '${env.path}'; exit \$__rc"),
            "ossfs2 conf and env must both be cleaned up regardless of ossfs2 exit code, got: $cmd",
        )
        assertCredentialsAbsentFromCommand(cmd)
    }

    @Test
    fun `ossfs2 mount encodes bucketDirectory as oss_bucket_prefix with trailing slash`() {
        val entriesSlot = slot<List<WriteEntry>>()
        every { fileSystemService.write(capture(entriesSlot)) } just Runs
        every { commandService.run(any<String>()) } returns successExecution()

        sandbox.mount(
            OssfsMountOptions.builder()
                .endpoint("https://oss-cn-hangzhou.aliyuncs.com")
                .bucket("b")
                .bucketDirectory("sub/dir")
                .mountPoint("/mnt/oss2")
                .accessKeyId("AK")
                .accessKeySecret("SK")
                .version(OssfsMountOptions.Version.OSSFS_2_0)
                .build(),
        )

        val conf = entriesSlot.captured.single { it.path.endsWith(".conf") }.data as String
        assertTrue(
            conf.contains("--oss_bucket_prefix=sub/dir/\n"),
            "ossfs2 subdir should map to oss_bucket_prefix with trailing slash, got: $conf",
        )
    }

    @Test
    fun `ossfs2 mount env file includes OSS_SESSION_TOKEN when securityToken is set`() {
        val entriesSlot = slot<List<WriteEntry>>()
        val cmdSlot = slot<String>()
        every { fileSystemService.write(capture(entriesSlot)) } just Runs
        every { commandService.run(capture(cmdSlot)) } returns successExecution()

        sandbox.mount(
            OssfsMountOptions.builder()
                .endpoint("https://oss.example.com")
                .bucket("b")
                .mountPoint("/mnt/oss2")
                .accessKeyId("AK")
                .accessKeySecret("SK")
                .securityToken("TOKEN")
                .version(OssfsMountOptions.Version.OSSFS_2_0)
                .build(),
        )
        val envBody =
            entriesSlot.captured.single { it.path.startsWith("/tmp/opensandbox-ossfsenv-") }.data as String
        assertTrue(envBody.contains("OSS_SESSION_TOKEN=TOKEN\n"), "env file must include STS token, got: $envBody")
        assertCredentialsAbsentFromCommand(cmdSlot.captured)
    }

    /**
     * Assert that the raw AK/SK/token strings used in tests never leak into the
     * generated mount command. Guards the P1 fix: shell command text is logged
     * by execd and must never contain credential material.
     */
    private fun assertCredentialsAbsentFromCommand(cmd: String) {
        assertTrue(!cmd.contains("'AK'"), "AK must not appear as a shell literal, got: $cmd")
        assertTrue(!cmd.contains("'SK'"), "SK must not appear as a shell literal, got: $cmd")
        assertTrue(!cmd.contains("'TOKEN'"), "TOKEN must not appear as a shell literal, got: $cmd")
        assertTrue(
            !cmd.contains("printf %s '"),
            "credentials must never be piped via printf into the command, got: $cmd",
        )
        assertTrue(
            !cmd.contains("export OSS_ACCESS_KEY_ID="),
            "credentials must not be exported inline in the command, got: $cmd",
        )
        assertTrue(
            !cmd.contains("export OSS_ACCESS_KEY_SECRET="),
            "credentials must not be exported inline in the command, got: $cmd",
        )
        assertTrue(
            !cmd.contains("export OSS_SESSION_TOKEN="),
            "credentials must not be exported inline in the command, got: $cmd",
        )
    }

    // -------- umount --------

    @Test
    fun `umount runs umount with quoted path`() {
        val cmdSlot = slot<String>()
        every { commandService.run(capture(cmdSlot)) } returns successExecution()
        sandbox.umount("/mnt/nas")
        assertEquals("umount '/mnt/nas'", cmdSlot.captured)
    }

    @Test
    fun `umount rejects blank`() {
        assertThrows(InvalidArgumentException::class.java) { sandbox.umount("   ") }
    }

    @Test
    fun `umount raises MountFailedException on error`() {
        every { commandService.run(any<String>()) } returns failedExecution(value = "not mounted")
        val ex = assertThrows(MountFailedException::class.java) { sandbox.umount("/mnt/nas") }
        assertTrue(ex.message!!.contains("umount failure"))
    }

    // -------- shell quoting --------

    @Test
    fun `shell-quoting handles embedded single quote safely`() {
        val cmdSlot = slot<String>()
        every { commandService.run(capture(cmdSlot)) } returns successExecution()
        // Feed a path with an embedded single quote so we exercise shQuote.
        sandbox.umount("/mnt/wei'rd")
        // /mnt/wei'rd → '/mnt/wei'\''rd'
        assertEquals("umount '/mnt/wei'\\''rd'", cmdSlot.captured)
    }

    @Suppress("unused")
    private fun clearAllMocksAfter() {
        // referenced to keep clearMocks import in case of future use
        clearMocks(commandService)
    }
}
