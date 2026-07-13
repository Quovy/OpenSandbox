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

namespace OpenSandbox.Models;

/// <summary>
/// Selects between ossfs 1.x and ossfs 2.x.
/// </summary>
public enum OssfsVersion
{
    /// <summary>ossfs 1.x</summary>
    Ossfs10,

    /// <summary>ossfs 2.x</summary>
    Ossfs20,
}

/// <summary>
/// Marker base type shared by <see cref="NfsMountOptions"/> and <see cref="OssfsMountOptions"/>.
/// Consumers should use the concrete option types; the base type is only used to
/// keep the extension method signatures narrow.
/// </summary>
public abstract class MountOptions
{
    internal MountOptions()
    {
    }
}

/// <summary>
/// Options describing an NFS mount executed inside a sandbox via
/// <c>sandbox.MountAsync(...)</c>.
/// </summary>
/// <remarks>
/// The SDK does not manage the NAS service. It assembles a <c>mount -t nfs</c>
/// shell command and runs it via <c>sandbox.Commands.RunAsync</c>. The sandbox
/// image must already contain the <c>mount.nfs</c> binary, or
/// <see cref="Installation"/> can install it at mount time.
/// </remarks>
public class NfsMountOptions : MountOptions
{
    /// <summary>
    /// Default NFSv3 option string applied when <see cref="Options"/> is null or blank.
    /// </summary>
    public const string DefaultNfsOptions =
        "vers=3,nolock,proto=tcp,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport";

    /// <summary>Gets or sets the NFS server endpoint (host or IP, without the export path).</summary>
    public required string Endpoint { get; set; }

    /// <summary>Gets or sets the export path on the NFS server (for example <c>/</c>).</summary>
    public required string NasPath { get; set; }

    /// <summary>Gets or sets the absolute path inside the sandbox where the export will be mounted.</summary>
    public required string MountPoint { get; set; }

    /// <summary>
    /// Gets or sets the comma-separated NFS mount options passed to <c>mount -o</c>.
    /// Defaults to <see cref="DefaultNfsOptions"/> when null or blank.
    /// </summary>
    public string? Options { get; set; }

    /// <summary>
    /// Gets or sets an optional shell command executed before the mount command,
    /// typically to install NFS client packages (for example <c>apt-get install -y nfs-common</c>).
    /// </summary>
    public string? Installation { get; set; }
}

/// <summary>
/// Options describing an Alibaba Cloud OSS mount executed inside a sandbox with
/// either <c>ossfs</c> (1.x) or <c>ossfs2</c> (2.x).
/// </summary>
/// <remarks>
/// When <see cref="Version"/> is null the SDK defaults to <see cref="OssfsVersion.Ossfs10"/>.
/// <see cref="BucketDirectory"/> is supported by both versions: ossfs 1.x mounts
/// <c>bucket:/dir</c>; ossfs 2.x writes it to the configuration file as
/// <c>--oss_bucket_prefix=&lt;dir&gt;/</c>.
/// </remarks>
public class OssfsMountOptions : MountOptions
{
    /// <summary>Gets or sets the OSS endpoint URL.</summary>
    public required string Endpoint { get; set; }

    /// <summary>Gets or sets the OSS bucket name.</summary>
    public required string Bucket { get; set; }

    /// <summary>Gets or sets the absolute path inside the sandbox where the bucket is mounted.</summary>
    public required string MountPoint { get; set; }

    /// <summary>Gets or sets the Alibaba Cloud access key id.</summary>
    public required string AccessKeyId { get; set; }

    /// <summary>Gets or sets the Alibaba Cloud access key secret.</summary>
    public required string AccessKeySecret { get; set; }

    /// <summary>
    /// Gets or sets the optional STS security token. When set, ossfs 1.x appends
    /// it to the password file and ossfs 2.x exports it as <c>OSS_SESSION_TOKEN</c>.
    /// </summary>
    public string? SecurityToken { get; set; }

    /// <summary>
    /// Gets or sets the ossfs major version. Defaults to <see cref="OssfsVersion.Ossfs10"/> when null.
    /// </summary>
    public OssfsVersion? Version { get; set; }

    /// <summary>Gets or sets an optional subdirectory inside the bucket to mount.</summary>
    public string? BucketDirectory { get; set; }

    /// <summary>
    /// Gets or sets the additional ossfs options. Each entry is the raw option
    /// value without the leading <c>-o</c> (ossfs 1.x) or <c>--</c> (ossfs 2.x)
    /// prefix, for example <c>"use_cache=/tmp/ossfs"</c>.
    /// </summary>
    public IReadOnlyList<string>? Options { get; set; }

    /// <summary>Gets or sets an optional shell command executed before the mount command.</summary>
    public string? Installation { get; set; }
}
