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

using System.Globalization;
using System.Text;
using OpenSandbox.Core;
using OpenSandbox.Models;

namespace OpenSandbox.Internal;

/// <summary>
/// Internal builders that translate mount option objects into shell commands.
/// Exposed as <c>internal</c> so the unit tests in <c>OpenSandbox.Tests</c> can
/// call them directly via <c>InternalsVisibleTo</c>.
/// </summary>
internal static class MountShell
{
    /// <summary>
    /// Prefix for the per-call ossfs 1.x password file under <c>/tmp</c>. A
    /// unique suffix is appended per mount so that concurrent ossfs 1.x
    /// mounts in the same sandbox do not overwrite or delete each other's
    /// credentials.
    /// </summary>
    public const string Ossfs1PasswdPathPrefix = "/tmp/opensandbox-ossfspass-";

    public sealed record Ossfs1Plan(string PasswdPath, string PasswdContent, string Command);

    public sealed record Ossfs2Plan(
        string ConfPath,
        string ConfContent,
        string EnvPath,
        string EnvContent,
        string Command);

    public static void ValidateNfs(NfsMountOptions options)
    {
        if (options is null)
        {
            throw new ArgumentNullException(nameof(options));
        }
        if (string.IsNullOrWhiteSpace(options.Endpoint))
        {
            throw new InvalidArgumentException("endpoint must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.MountPoint))
        {
            throw new InvalidArgumentException("mountPoint must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.NasPath))
        {
            throw new InvalidArgumentException("nasPath must not be blank");
        }
    }

    public static void ValidateOssfs(OssfsMountOptions options)
    {
        if (options is null)
        {
            throw new ArgumentNullException(nameof(options));
        }
        if (string.IsNullOrWhiteSpace(options.Endpoint))
        {
            throw new InvalidArgumentException("endpoint must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.Bucket))
        {
            throw new InvalidArgumentException("bucket must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.MountPoint))
        {
            throw new InvalidArgumentException("mountPoint must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.AccessKeyId))
        {
            throw new InvalidArgumentException("accessKeyId must not be blank");
        }
        if (string.IsNullOrWhiteSpace(options.AccessKeySecret))
        {
            throw new InvalidArgumentException("accessKeySecret must not be blank");
        }
    }

    public static string BuildNfsCommand(NfsMountOptions options)
    {
        var optString = string.IsNullOrWhiteSpace(options.Options)
            ? NfsMountOptions.DefaultNfsOptions
            : options.Options!;
        var source = options.Endpoint + ":" + options.NasPath;
        var core = string.Format(
            CultureInfo.InvariantCulture,
            "mkdir -p {0} && mount -t nfs -o {1} {2} {3}",
            ShQuote(options.MountPoint),
            ShQuote(optString),
            ShQuote(source),
            ShQuote(options.MountPoint));
        return PrependInstallation(options.Installation, core);
    }

    public static Ossfs1Plan BuildOssfs1Plan(OssfsMountOptions options)
    {
        var useSts = !string.IsNullOrWhiteSpace(options.SecurityToken);
        // ossfs 1.x password file format:
        //   AK/SK mode : bucket:accessKeyId:accessKeySecret
        //   STS mode   : bucket:accessKeyId:accessKeySecret:securityToken
        var passwd = new StringBuilder();
        passwd.Append(options.Bucket).Append(':')
              .Append(options.AccessKeyId).Append(':')
              .Append(options.AccessKeySecret);
        if (useSts)
        {
            passwd.Append(':').Append(options.SecurityToken);
        }

        var bucketArg = string.IsNullOrWhiteSpace(options.BucketDirectory)
            ? options.Bucket
            : $"{options.Bucket}:/{options.BucketDirectory}";

        var optionFlags = new StringBuilder();
        if (options.Options is { Count: > 0 } opts)
        {
            foreach (var opt in opts)
            {
                optionFlags.Append(" -o").Append(ShQuote(opt));
            }
        }

        // Upload the password file via the filesystem API so credentials never
        // appear in the shell command (which execd logs). Use a unique
        // per-call path under /tmp so concurrent ossfs 1.x mounts do not race
        // on the same file. Always clean it up even if ossfs fails, by
        // preserving the subshell exit code via __rc.
        var passwdPath = Ossfs1PasswdPathPrefix + Guid.NewGuid().ToString("N");
        var quotedPasswdPath = ShQuote(passwdPath);
        var core = string.Format(
            CultureInfo.InvariantCulture,
            "ossfs --version && " +
            "mkdir -p {0} && " +
            "( ossfs {1} {0} -ourl={2} -opasswd_file={3}{4} ); " +
            "__rc=$?; rm -f {3}; exit $__rc",
            ShQuote(options.MountPoint),
            ShQuote(bucketArg),
            ShQuote(options.Endpoint),
            quotedPasswdPath,
            optionFlags);
        return new Ossfs1Plan(
            PasswdPath: passwdPath,
            PasswdContent: passwd.ToString(),
            Command: PrependInstallation(options.Installation, core));
    }

    public static WriteEntry BuildOssfs1PasswdEntry(Ossfs1Plan plan)
    {
        if (plan is null)
        {
            throw new ArgumentNullException(nameof(plan));
        }
        return new WriteEntry
        {
            Path = plan.PasswdPath,
            Data = plan.PasswdContent,
            Mode = 600,
        };
    }

    public static Ossfs2Plan BuildOssfs2Plan(OssfsMountOptions options)
    {
        var conf = new StringBuilder();
        conf.Append("--oss_endpoint=").Append(options.Endpoint).Append('\n');
        conf.Append("--oss_bucket=").Append(options.Bucket).Append('\n');
        if (!string.IsNullOrWhiteSpace(options.BucketDirectory))
        {
            // ossfs2 mounts a bucket root; a subdirectory is expressed as a
            // prefix (trailing slash makes it a directory boundary).
            var prefix = options.BucketDirectory!.TrimEnd('/') + "/";
            conf.Append("--oss_bucket_prefix=").Append(prefix).Append('\n');
        }
        if (options.Options is { Count: > 0 } opts)
        {
            foreach (var opt in opts)
            {
                conf.Append("--").Append(opt).Append('\n');
            }
        }

        // ossfs 2.x reads credentials from environment variables. Deliver
        // them via a separate env file so they never appear in the shell
        // command text (which execd logs). ossfs2 picks them up after the
        // command sources the file with `set -a`.
        var env = new StringBuilder();
        env.Append("OSS_ACCESS_KEY_ID=").Append(options.AccessKeyId).Append('\n');
        env.Append("OSS_ACCESS_KEY_SECRET=").Append(options.AccessKeySecret).Append('\n');
        if (!string.IsNullOrWhiteSpace(options.SecurityToken))
        {
            env.Append("OSS_SESSION_TOKEN=").Append(options.SecurityToken).Append('\n');
        }

        var confPath = "/tmp/opensandbox-ossfs-" + Guid.NewGuid().ToString("N") + ".conf";
        var envPath = "/tmp/opensandbox-ossfsenv-" + Guid.NewGuid().ToString("N");
        var quotedConfPath = ShQuote(confPath);
        var quotedEnvPath = ShQuote(envPath);

        // Always remove both files after the mount attempt, even on failure,
        // so repeated mounts do not accumulate credential-adjacent files in
        // /tmp. The subshell preserves the ossfs2 exit code via __rc.
        var core = string.Format(
            CultureInfo.InvariantCulture,
            "ossfs2 --version && " +
            "mkdir -p {0} && " +
            "set -a && . {2} && set +a && " +
            "( ossfs2 mount {0} -c {1} ); " +
            "__rc=$?; rm -f {1} {2}; exit $__rc",
            ShQuote(options.MountPoint),
            quotedConfPath,
            quotedEnvPath);

        return new Ossfs2Plan(
            ConfPath: confPath,
            ConfContent: conf.ToString(),
            EnvPath: envPath,
            EnvContent: env.ToString(),
            Command: PrependInstallation(options.Installation, core));
    }

    public static IReadOnlyList<WriteEntry> BuildOssfs2Entries(Ossfs2Plan plan)
    {
        if (plan is null)
        {
            throw new ArgumentNullException(nameof(plan));
        }
        return new WriteEntry[]
        {
            new()
            {
                Path = plan.ConfPath,
                Data = plan.ConfContent,
                // Mode is serialized as a JSON number and parsed by execd as
                // an octal string (see
                // components/execd/pkg/web/controller/utils.go); pass the
                // literal decimal 600 rather than the C# hex/binary form for
                // octal 0o600.
                Mode = 600,
            },
            new()
            {
                Path = plan.EnvPath,
                Data = plan.EnvContent,
                Mode = 600,
            },
        };
    }

    public static string BuildUmountCommand(string mountPoint)
    {
        if (string.IsNullOrWhiteSpace(mountPoint))
        {
            throw new InvalidArgumentException("mountPoint must not be blank");
        }
        return "umount " + ShQuote(mountPoint);
    }

    public static OssfsVersion SelectOssfsVersion(OssfsMountOptions options)
    {
        return options.Version ?? OssfsVersion.Ossfs10;
    }

    public static void EnsureSuccess(Execution? execution, string failurePrefix)
    {
        if (execution is null)
        {
            throw new MountFailedException(failurePrefix + ": nil execution result");
        }
        var error = execution.Error;
        var exitCode = execution.ExitCode;
        var failed = error != null || (exitCode.HasValue && exitCode.Value != 0);
        if (!failed)
        {
            return;
        }
        var parts = new List<string>(2);
        if (error != null)
        {
            parts.Add($"[{error.Name}] {error.Value}");
        }
        if (execution.Logs.Stderr.Count > 0)
        {
            var sb = new StringBuilder();
            for (var i = 0; i < execution.Logs.Stderr.Count; i++)
            {
                if (i > 0)
                {
                    sb.Append('\n');
                }
                sb.Append(execution.Logs.Stderr[i].Text);
            }
            parts.Add("stderr=" + sb);
        }
        var message = parts.Count == 0
            ? failurePrefix
            : failurePrefix + ": " + string.Join(" | ", parts);
        throw new MountFailedException(message, execution);
    }

    private static string PrependInstallation(string? installation, string core)
    {
        return string.IsNullOrWhiteSpace(installation) ? core : installation + " && " + core;
    }

    /// <summary>
    /// Quotes <paramref name="value"/> for POSIX shell single-quoted context.
    /// Embedded <c>'</c> is escaped as <c>'\''</c>. The result is always safe to
    /// embed as one argument to a <c>sh -c</c> string.
    /// </summary>
    public static string ShQuote(string value)
    {
        if (value is null)
        {
            throw new ArgumentNullException(nameof(value));
        }
        // string.Replace(string, string) is available on all target frameworks
        // (including netstandard2.0). Ordinal comparison is the default for
        // this overload.
        return "'" + value.Replace("'", "'\\''") + "'";
    }
}
