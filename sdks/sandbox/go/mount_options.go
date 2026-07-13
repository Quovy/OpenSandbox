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

package opensandbox

// MountOptions is a sealed marker interface implemented by NfsMountOptions and
// OssfsMountOptions. It exists so that Sandbox.Mount can accept either kind of
// options while still failing fast for anything else.
type MountOptions interface {
	isMountOptions()
}

// NfsMountOptions describes an NFS mount executed inside a sandbox via
// Sandbox.Mount. The SDK does not manage the NAS service. It assembles a
// `mount -t nfs` shell command and runs it via Sandbox.RunCommand. The sandbox
// image must already contain the `mount.nfs` binary, or Installation can be
// used to install it at mount time.
type NfsMountOptions struct {
	// Endpoint is the NFS server endpoint (host or IP, without the export path).
	Endpoint string
	// NasPath is the export path on the NFS server (for example "/").
	NasPath string
	// MountPoint is the absolute path inside the sandbox where the export will
	// be mounted. The directory is created if it does not exist.
	MountPoint string
	// Options is the comma-separated NFS mount option string passed to
	// `mount -o`. When empty, DefaultNfsOptions is used.
	Options string
	// Installation is an optional shell command executed before the mount
	// command, typically to install NFS client packages.
	Installation string
}

func (*NfsMountOptions) isMountOptions() {}

// OssfsVersion selects between ossfs 1.x and ossfs 2.x.
type OssfsVersion string

const (
	OssfsVersion10 OssfsVersion = "1.0"
	OssfsVersion20 OssfsVersion = "2.0"
)

// OssfsMountOptions describes an Alibaba Cloud OSS mount executed inside a
// sandbox with either `ossfs` (1.x) or `ossfs2` (2.x), selected by Version.
//
// BucketDirectory is supported by both versions:
//   - ossfs 1.x: mounts "bucket:/dir".
//   - ossfs 2.x: written to the configuration file as
//     "--oss_bucket_prefix=<dir>/".
type OssfsMountOptions struct {
	// Endpoint is the OSS endpoint URL, for example
	// "https://oss-cn-hangzhou.aliyuncs.com".
	Endpoint string
	// Bucket is the OSS bucket name.
	Bucket string
	// MountPoint is the absolute path inside the sandbox where the bucket will
	// be mounted.
	MountPoint string
	// AccessKeyID is the Alibaba Cloud access key id.
	AccessKeyID string
	// AccessKeySecret is the Alibaba Cloud access key secret.
	AccessKeySecret string
	// SecurityToken is an optional STS security token. When set, ossfs 1.x
	// appends it to the password file; ossfs 2.x exports it as
	// OSS_SESSION_TOKEN.
	SecurityToken string
	// Version selects ossfs 1.x or ossfs 2.x. When empty, OssfsVersion10 is
	// used.
	Version OssfsVersion
	// BucketDirectory is an optional subdirectory inside the bucket to mount.
	BucketDirectory string
	// Options is an optional list of ossfs option values, each without the
	// leading `-o` (ossfs 1.x) or `--` (ossfs 2.x) prefix.
	Options []string
	// Installation is an optional shell command executed before the mount
	// command, typically to install `ossfs` / `ossfs2`.
	Installation string
}

func (*OssfsMountOptions) isMountOptions() {}
