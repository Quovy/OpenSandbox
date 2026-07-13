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

using FluentAssertions;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class MountShellTests
{
    // -------- shell quoting --------

    [Fact]
    public void ShQuote_QuotesSimplePath()
    {
        MountShell.ShQuote("/mnt/nas").Should().Be("'/mnt/nas'");
    }

    [Fact]
    public void ShQuote_EscapesEmbeddedSingleQuotes()
    {
        // /mnt/wei'rd -> '/mnt/wei'\''rd'
        MountShell.ShQuote("/mnt/wei'rd").Should().Be("'/mnt/wei'\\''rd'");
    }

    // -------- NFS builder --------

    [Fact]
    public void BuildNfsCommand_UsesDefaultOptionsAndQuotesPaths()
    {
        var cmd = MountShell.BuildNfsCommand(new NfsMountOptions
        {
            Endpoint = "nas-server.example.com",
            NasPath = "/share",
            MountPoint = "/mnt/nas",
        });
        cmd.Should().Contain("mkdir -p '/mnt/nas'");
        cmd.Should().Contain($"mount -t nfs -o '{NfsMountOptions.DefaultNfsOptions}' 'nas-server.example.com:/share' '/mnt/nas'");
    }

    [Fact]
    public void BuildNfsCommand_HonorsCustomOptionsAndPrependsInstallation()
    {
        var cmd = MountShell.BuildNfsCommand(new NfsMountOptions
        {
            Endpoint = "nas",
            NasPath = "/",
            MountPoint = "/mnt/x",
            Options = "vers=4,proto=tcp",
            Installation = "apt-get install -y nfs-common",
        });
        cmd.Should().StartWith("apt-get install -y nfs-common && ");
        cmd.Should().Contain("'vers=4,proto=tcp'");
        cmd.Should().NotContain(NfsMountOptions.DefaultNfsOptions);
    }

    [Theory]
    [InlineData("   ", "/", "/mnt/x")]
    [InlineData("nas", "", "/mnt/x")]
    [InlineData("nas", "/", " ")]
    public void ValidateNfs_RejectsBlankFields(string endpoint, string nasPath, string mountPoint)
    {
        var options = new NfsMountOptions
        {
            Endpoint = endpoint,
            NasPath = nasPath,
            MountPoint = mountPoint,
        };
        Action act = () => MountShell.ValidateNfs(options);
        act.Should().Throw<InvalidArgumentException>();
    }

    // -------- ossfs 1.x plan --------

    /// <summary>
    /// Guard: shell command text is logged by execd, so raw AK/SK/token
    /// strings must never appear in it.
    /// </summary>
    private static void AssertNoCredentialsInCommand(string cmd)
    {
        cmd.Should().NotContain("printf %s",
            because: "credentials must not be piped via printf");
        cmd.Should().NotContain("install -m 600 /dev/null",
            because: "credentials must not be inlined via install/printf");
        cmd.Should().NotContain("export OSS_ACCESS_KEY_ID=",
            because: "credentials must not be exported inline");
        cmd.Should().NotContain("export OSS_ACCESS_KEY_SECRET=",
            because: "credentials must not be exported inline");
        cmd.Should().NotContain("export OSS_SESSION_TOKEN=",
            because: "credentials must not be exported inline");
    }

    [Fact]
    public void BuildOssfs1Plan_UploadsPasswdFileAndCleansUp()
    {
        var plan = MountShell.BuildOssfs1Plan(new OssfsMountOptions
        {
            Endpoint = "https://oss-cn-hangzhou.aliyuncs.com",
            Bucket = "my-bucket",
            MountPoint = "/mnt/oss",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
        });
        plan.PasswdPath.Should().StartWith(MountShell.Ossfs1PasswdPathPrefix);
        plan.PasswdContent.Should().Be("my-bucket:AK:SK");

        var cmd = plan.Command;
        var quotedPasswdPath = $"'{plan.PasswdPath}'";
        cmd.Should().Contain("ossfs 'my-bucket' '/mnt/oss'");
        cmd.Should().Contain("-ourl='https://oss-cn-hangzhou.aliyuncs.com'");
        cmd.Should().Contain($"-opasswd_file={quotedPasswdPath}");
        cmd.Should().Contain($"__rc=$?; rm -f {quotedPasswdPath}; exit $__rc",
            because: "ossfs1 must always clean up the password file regardless of ossfs exit code");
        AssertNoCredentialsInCommand(cmd);
    }

    [Fact]
    public void BuildOssfs1PasswdEntry_UsesMode600()
    {
        var plan = MountShell.BuildOssfs1Plan(new OssfsMountOptions
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
        });
        var entry = MountShell.BuildOssfs1PasswdEntry(plan);
        entry.Path.Should().Be(plan.PasswdPath);
        entry.Data.Should().Be(plan.PasswdContent);
        entry.Mode.Should().Be(600);
    }

    [Fact]
    public void BuildOssfs1Plan_UsesDistinctPasswdPathPerCall()
    {
        OssfsMountOptions Options() => new()
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
        };
        var a = MountShell.BuildOssfs1Plan(Options()).PasswdPath;
        var b = MountShell.BuildOssfs1Plan(Options()).PasswdPath;
        a.Should().NotBeNullOrEmpty();
        b.Should().NotBeNullOrEmpty();
        a.Should().NotBe(b, because: "concurrent ossfs1 mounts must not share the same passwd file");
    }

    [Fact]
    public void BuildOssfs1Plan_SupportsStsAndBucketDirectoryAndOptions()
    {
        var plan = MountShell.BuildOssfs1Plan(new OssfsMountOptions
        {
            Endpoint = "https://oss.example.com",
            Bucket = "b",
            BucketDirectory = "subdir",
            MountPoint = "/mnt/oss",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
            SecurityToken = "TOKEN",
            Options = new[] { "use_cache=/tmp/ossfs", "allow_other" },
            Version = OssfsVersion.Ossfs10,
        });
        plan.PasswdContent.Should().Be("b:AK:SK:TOKEN");
        var cmd = plan.Command;
        cmd.Should().Contain("ossfs 'b:/subdir' '/mnt/oss'");
        cmd.Should().Contain("-o'use_cache=/tmp/ossfs'");
        cmd.Should().Contain("-o'allow_other'");
        AssertNoCredentialsInCommand(cmd);
    }

    [Fact]
    public void ValidateOssfs_RejectsBlankCredentials()
    {
        OssfsMountOptions Base() => new()
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
        };

        var missingAk = Base();
        missingAk.AccessKeyId = "  ";
        Assert.Throws<InvalidArgumentException>(() => MountShell.ValidateOssfs(missingAk));

        var missingSk = Base();
        missingSk.AccessKeySecret = string.Empty;
        Assert.Throws<InvalidArgumentException>(() => MountShell.ValidateOssfs(missingSk));

        var missingBucket = Base();
        missingBucket.Bucket = string.Empty;
        Assert.Throws<InvalidArgumentException>(() => MountShell.ValidateOssfs(missingBucket));
    }

    // -------- ossfs 2.x plan --------

    [Fact]
    public void BuildOssfs2Plan_UploadsConfAndEnvFilesAndCleansUp()
    {
        var plan = MountShell.BuildOssfs2Plan(new OssfsMountOptions
        {
            Endpoint = "https://oss.example.com",
            Bucket = "b",
            MountPoint = "/mnt/oss2",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
            Options = new[] { "cache_dir=/tmp/ossfs2" },
            Version = OssfsVersion.Ossfs20,
        });
        plan.ConfPath.Should().StartWith("/tmp/opensandbox-ossfs-");
        plan.ConfPath.Should().EndWith(".conf");
        plan.EnvPath.Should().StartWith("/tmp/opensandbox-ossfsenv-");

        plan.ConfContent.Should().Contain("--oss_endpoint=https://oss.example.com\n");
        plan.ConfContent.Should().Contain("--oss_bucket=b\n");
        plan.ConfContent.Should().Contain("--cache_dir=/tmp/ossfs2\n");

        plan.EnvContent.Should().Contain("OSS_ACCESS_KEY_ID=AK\n");
        plan.EnvContent.Should().Contain("OSS_ACCESS_KEY_SECRET=SK\n");

        plan.Command.Should().Contain("ossfs2 --version");
        plan.Command.Should().Contain($"set -a && . '{plan.EnvPath}' && set +a");
        plan.Command.Should().Contain($"( ossfs2 mount '/mnt/oss2' -c '{plan.ConfPath}' )");
        plan.Command.Should().Contain(
            $"__rc=$?; rm -f '{plan.ConfPath}' '{plan.EnvPath}'; exit $__rc",
            because: "ossfs2 conf and env must both be cleaned up regardless of ossfs2 exit code");
        AssertNoCredentialsInCommand(plan.Command);
    }

    [Fact]
    public void BuildOssfs2Plan_EncodesBucketDirectoryAsPrefixWithTrailingSlash()
    {
        var plan = MountShell.BuildOssfs2Plan(new OssfsMountOptions
        {
            Endpoint = "e",
            Bucket = "b",
            BucketDirectory = "sub/dir",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
            Version = OssfsVersion.Ossfs20,
        });
        plan.ConfContent.Should().Contain("--oss_bucket_prefix=sub/dir/\n");
    }

    [Fact]
    public void BuildOssfs2Plan_EnvIncludesSessionTokenWhenSet()
    {
        var plan = MountShell.BuildOssfs2Plan(new OssfsMountOptions
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
            SecurityToken = "TOKEN",
            Version = OssfsVersion.Ossfs20,
        });
        plan.EnvContent.Should().Contain("OSS_SESSION_TOKEN=TOKEN\n");
        AssertNoCredentialsInCommand(plan.Command);
    }

    [Fact]
    public void BuildOssfs2Entries_ReturnsConfAndEnvWithMode600()
    {
        var plan = MountShell.BuildOssfs2Plan(new OssfsMountOptions
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
            Version = OssfsVersion.Ossfs20,
        });
        var entries = MountShell.BuildOssfs2Entries(plan);
        entries.Should().HaveCount(2, "ossfs2 must upload both conf and env files");
        var byPath = entries.ToDictionary(e => e.Path);
        byPath[plan.ConfPath].Data.Should().Be(plan.ConfContent);
        byPath[plan.ConfPath].Mode.Should().Be(600);
        byPath[plan.EnvPath].Data.Should().Be(plan.EnvContent);
        byPath[plan.EnvPath].Mode.Should().Be(600);
    }

    [Fact]
    public void SelectOssfsVersion_DefaultsToV10()
    {
        var opts = new OssfsMountOptions
        {
            Endpoint = "e",
            Bucket = "b",
            MountPoint = "/mnt/x",
            AccessKeyId = "AK",
            AccessKeySecret = "SK",
        };
        MountShell.SelectOssfsVersion(opts).Should().Be(OssfsVersion.Ossfs10);
        opts.Version = OssfsVersion.Ossfs20;
        MountShell.SelectOssfsVersion(opts).Should().Be(OssfsVersion.Ossfs20);
    }

    // -------- umount --------

    [Fact]
    public void BuildUmountCommand_QuotesPath()
    {
        MountShell.BuildUmountCommand("/mnt/nas").Should().Be("umount '/mnt/nas'");
    }

    [Fact]
    public void BuildUmountCommand_RejectsBlank()
    {
        Assert.Throws<InvalidArgumentException>(() => MountShell.BuildUmountCommand("   "));
    }

    // -------- EnsureSuccess --------

    [Fact]
    public void EnsureSuccess_PassesForZeroExit()
    {
        var exec = new Execution();
        exec.ExitCode = 0;
        MountShell.EnsureSuccess(exec, "prefix");
    }

    [Fact]
    public void EnsureSuccess_ThrowsWithExecutionAttachedOnError()
    {
        var exec = new Execution
        {
            ExitCode = 1,
            Error = new ExecutionError
            {
                Name = "MountError",
                Value = "denied",
                Timestamp = 0,
                Traceback = Array.Empty<string>(),
            },
        };
        exec.Logs.Stderr.Add(new OutputMessage { Text = "stderr text", Timestamp = 0 });

        var ex = Assert.Throws<MountFailedException>(() => MountShell.EnsureSuccess(exec, "NAS mount failure"));
        ex.Message.Should().Contain("NAS mount failure");
        ex.Message.Should().Contain("MountError");
        ex.Message.Should().Contain("stderr=stderr text");
        ex.Execution.Should().BeSameAs(exec);
        ex.Error.Code.Should().Be(SandboxErrorCodes.MountFailed);
    }

    [Fact]
    public void EnsureSuccess_ThrowsOnNonZeroExitWithoutErrorField()
    {
        var exec = new Execution { ExitCode = 2 };
        Assert.Throws<MountFailedException>(() => MountShell.EnsureSuccess(exec, "prefix"));
    }

    [Fact]
    public void EnsureSuccess_ThrowsOnNullExecution()
    {
        Assert.Throws<MountFailedException>(() => MountShell.EnsureSuccess(null, "prefix"));
    }
}
