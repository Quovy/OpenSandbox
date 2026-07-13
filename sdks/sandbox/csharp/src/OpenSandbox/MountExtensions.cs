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

using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;

namespace OpenSandbox;

/// <summary>
/// Extension methods that add NAS/OSS mount syntax sugar to <see cref="Sandbox"/>.
/// </summary>
/// <remarks>
/// These helpers do not manage the remote NAS/OSS service. They assemble the
/// appropriate <c>mount -t nfs</c> / <c>ossfs</c> / <c>ossfs2</c> shell command
/// and run it via <see cref="Sandbox.Commands"/>. The sandbox image must have
/// the corresponding mount binary installed, or <c>Installation</c> can be used
/// to install it at mount time.
/// </remarks>
public static class MountExtensions
{
    /// <summary>
    /// Mount a NAS export inside the sandbox by running <c>mount -t nfs</c>.
    /// </summary>
    /// <param name="sandbox">The sandbox to mount into.</param>
    /// <param name="options">The NFS mount options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The <see cref="Execution"/> from the underlying <c>RunAsync</c> call.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="options"/> is invalid.</exception>
    /// <exception cref="MountFailedException">Thrown when the mount command fails inside the sandbox.</exception>
    public static async Task<Execution> MountAsync(
        this Sandbox sandbox,
        NfsMountOptions options,
        CancellationToken cancellationToken = default)
    {
        if (sandbox is null)
        {
            throw new ArgumentNullException(nameof(sandbox));
        }
        if (options is null)
        {
            throw new ArgumentNullException(nameof(options));
        }
        MountShell.ValidateNfs(options);
        var execution = await sandbox.Commands
            .RunAsync(MountShell.BuildNfsCommand(options), cancellationToken: cancellationToken)
            .ConfigureAwait(false);
        MountShell.EnsureSuccess(execution, "NAS mount failure");
        return execution;
    }

    /// <summary>
    /// Mount an Alibaba Cloud OSS bucket inside the sandbox using either
    /// <c>ossfs</c> (1.x) or <c>ossfs2</c> (2.x), selected by
    /// <see cref="OssfsMountOptions.Version"/>.
    /// </summary>
    /// <param name="sandbox">The sandbox to mount into.</param>
    /// <param name="options">The OSS mount options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The <see cref="Execution"/> from the underlying <c>RunAsync</c> call.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="options"/> is invalid.</exception>
    /// <exception cref="MountFailedException">Thrown when the mount command fails inside the sandbox.</exception>
    public static async Task<Execution> MountAsync(
        this Sandbox sandbox,
        OssfsMountOptions options,
        CancellationToken cancellationToken = default)
    {
        if (sandbox is null)
        {
            throw new ArgumentNullException(nameof(sandbox));
        }
        if (options is null)
        {
            throw new ArgumentNullException(nameof(options));
        }
        MountShell.ValidateOssfs(options);
        var version = MountShell.SelectOssfsVersion(options);
        if (version == OssfsVersion.Ossfs10)
        {
            var plan1 = MountShell.BuildOssfs1Plan(options);
            await sandbox.Files
                .WriteFilesAsync(
                    new[] { MountShell.BuildOssfs1PasswdEntry(plan1) },
                    cancellationToken)
                .ConfigureAwait(false);
            var execution = await sandbox.Commands
                .RunAsync(plan1.Command, cancellationToken: cancellationToken)
                .ConfigureAwait(false);
            MountShell.EnsureSuccess(execution, "ossfs1.0 mount failure");
            return execution;
        }
        var plan = MountShell.BuildOssfs2Plan(options);
        await sandbox.Files
            .WriteFilesAsync(MountShell.BuildOssfs2Entries(plan), cancellationToken)
            .ConfigureAwait(false);
        var exec = await sandbox.Commands
            .RunAsync(plan.Command, cancellationToken: cancellationToken)
            .ConfigureAwait(false);
        MountShell.EnsureSuccess(exec, "ossfs2.0 mount failure");
        return exec;
    }

    /// <summary>
    /// Unmount a previously mounted path inside the sandbox by running <c>umount</c>.
    /// </summary>
    /// <param name="sandbox">The sandbox to unmount from.</param>
    /// <param name="mountPoint">The absolute path inside the sandbox to unmount.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The <see cref="Execution"/> from the underlying <c>RunAsync</c> call.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when <paramref name="mountPoint"/> is blank.</exception>
    /// <exception cref="MountFailedException">Thrown when the umount command fails.</exception>
    public static async Task<Execution> UmountAsync(
        this Sandbox sandbox,
        string mountPoint,
        CancellationToken cancellationToken = default)
    {
        if (sandbox is null)
        {
            throw new ArgumentNullException(nameof(sandbox));
        }
        var command = MountShell.BuildUmountCommand(mountPoint);
        var execution = await sandbox.Commands
            .RunAsync(command, cancellationToken: cancellationToken)
            .ConfigureAwait(false);
        MountShell.EnsureSuccess(execution, "umount failure");
        return execution;
    }
}
