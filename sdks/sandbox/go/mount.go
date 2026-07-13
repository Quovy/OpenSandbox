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

// This file exposes NAS/OSS mount syntax sugar on top of Sandbox.RunCommand.
// See Sandbox.Mount and Sandbox.Umount below for the public entrypoints and
// mount_options.go for the option types.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// DefaultNfsOptions is the option string applied to NfsMountOptions.Options
// when it is empty. It matches the NFSv3 profile used by the Alibaba internal
// SDK for compatibility with NAS servers deployed in production.
const DefaultNfsOptions = "vers=3,nolock,proto=tcp,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport"

// ossfs1PasswdPathPrefix is the prefix used to build a per-call ossfs 1.x
// password file under /tmp. A random suffix is appended per mount so that
// concurrent ossfs 1.x mounts in the same sandbox do not overwrite or delete
// each other's credentials. Kept as a package-level constant so tests can
// assert on the exact command emitted by the builders.
const ossfs1PasswdPathPrefix = "/tmp/opensandbox-ossfspass-"

// MountFailedError is returned when a NAS or OSS mount syntax-sugar call fails
// inside the sandbox. The failing Execution is attached so callers can inspect
// exit code, stdout and stderr for diagnostics.
type MountFailedError struct {
	Message   string
	Execution *Execution
}

// Error implements the error interface.
func (e *MountFailedError) Error() string {
	if e == nil {
		return "opensandbox: mount failed"
	}
	return e.Message
}

// Mount runs a NAS or OSS mount command inside this sandbox. The option
// argument selects the backend: pass a *NfsMountOptions for NFS or
// *OssfsMountOptions for OSS (ossfs 1.x / 2.x). Both option types are
// documented in mount_options.go.
//
// The sandbox image must have the corresponding mount binary installed. Use
// the Installation field on the options to install it at mount time.
//
// Returns the Execution from the underlying RunCommand call. If the mount
// command fails (non-zero exit or streamed error event) a *MountFailedError
// is returned; the Execution is attached to the error for diagnostics.
func (s *Sandbox) Mount(ctx context.Context, options MountOptions) (*Execution, error) {
	if s == nil {
		return nil, &InvalidArgumentError{Field: "sandbox", Message: "nil sandbox receiver"}
	}
	switch opt := options.(type) {
	case *NfsMountOptions:
		if opt == nil {
			return nil, &InvalidArgumentError{Field: "options", Message: "nil NfsMountOptions"}
		}
		if err := validateNfs(opt); err != nil {
			return nil, err
		}
		cmd := buildNfsCommand(opt)
		return s.runMount(ctx, cmd, "NAS mount failure")
	case *OssfsMountOptions:
		if opt == nil {
			return nil, &InvalidArgumentError{Field: "options", Message: "nil OssfsMountOptions"}
		}
		if err := validateOssfs(opt); err != nil {
			return nil, err
		}
		switch selectOssfsVersion(opt) {
		case OssfsVersion10:
			return s.mountOssfs1(ctx, opt)
		case OssfsVersion20:
			return s.mountOssfs2(ctx, opt)
		default:
			return nil, &InvalidArgumentError{Field: "options.Version", Message: fmt.Sprintf("unsupported ossfs version: %q", opt.Version)}
		}
	default:
		return nil, &InvalidArgumentError{Field: "options", Message: fmt.Sprintf("unsupported mount options type: %T", options)}
	}
}

// Umount unmounts a previously mounted path inside this sandbox by running
// `umount <mount_point>`.
func (s *Sandbox) Umount(ctx context.Context, mountPoint string) (*Execution, error) {
	if s == nil {
		return nil, &InvalidArgumentError{Field: "sandbox", Message: "nil sandbox receiver"}
	}
	cmd, err := buildUmountCommand(mountPoint)
	if err != nil {
		return nil, err
	}
	return s.runMount(ctx, cmd, "umount failure")
}

func (s *Sandbox) mountOssfs1(ctx context.Context, opt *OssfsMountOptions) (*Execution, error) {
	plan := buildOssfs1Plan(opt)
	if err := s.UploadFiles(ctx, []UploadFileEntry{buildOssfs1PasswdEntry(plan)}); err != nil {
		return nil, fmt.Errorf("opensandbox: upload ossfs1 passwd: %w", err)
	}
	return s.runMount(ctx, plan.Command, "ossfs1.0 mount failure")
}

func (s *Sandbox) mountOssfs2(ctx context.Context, opt *OssfsMountOptions) (*Execution, error) {
	plan := buildOssfs2Plan(opt)
	if err := s.UploadFiles(ctx, buildOssfs2Entries(plan)); err != nil {
		return nil, fmt.Errorf("opensandbox: upload ossfs2 conf/env: %w", err)
	}
	return s.runMount(ctx, plan.Command, "ossfs2.0 mount failure")
}

func (s *Sandbox) runMount(ctx context.Context, command string, failurePrefix string) (*Execution, error) {
	execution, err := s.RunCommand(ctx, command, nil)
	if err != nil {
		return execution, err
	}
	if err := ensureMountSuccess(execution, failurePrefix); err != nil {
		return execution, err
	}
	return execution, nil
}

// --- builders (also called directly from tests) ---

// ossfs1Plan describes the ossfs 1.x mount.
//
// The password file (bucket:AK:SK[:token]) is delivered as PasswdContent; the
// caller must upload it via the filesystem API (mode 0600) before running
// Command. Because the credentials only travel through the filesystem write,
// the generated shell command references the path via -opasswd_file but never
// embeds AK/SK, so execd's command log cannot capture them.
type ossfs1Plan struct {
	PasswdPath    string
	PasswdContent string
	Command       string
}

// ossfs2Plan describes the ossfs 2.x mount.
//
// Both the ossfs2 conf file (endpoint / bucket / options) and a separate env
// file that carries the AK/SK[/session token] are uploaded through the
// filesystem API (mode 0600). The mount command sources the env file, so no
// credential ever appears in the command text (which execd logs).
type ossfs2Plan struct {
	ConfPath    string
	ConfContent string
	EnvPath     string
	EnvContent  string
	Command     string
}

func validateNfs(opt *NfsMountOptions) error {
	if strings.TrimSpace(opt.Endpoint) == "" {
		return &InvalidArgumentError{Field: "Endpoint", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.MountPoint) == "" {
		return &InvalidArgumentError{Field: "MountPoint", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.NasPath) == "" {
		return &InvalidArgumentError{Field: "NasPath", Message: "must not be blank"}
	}
	return nil
}

func validateOssfs(opt *OssfsMountOptions) error {
	if strings.TrimSpace(opt.Endpoint) == "" {
		return &InvalidArgumentError{Field: "Endpoint", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.Bucket) == "" {
		return &InvalidArgumentError{Field: "Bucket", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.MountPoint) == "" {
		return &InvalidArgumentError{Field: "MountPoint", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.AccessKeyID) == "" {
		return &InvalidArgumentError{Field: "AccessKeyID", Message: "must not be blank"}
	}
	if strings.TrimSpace(opt.AccessKeySecret) == "" {
		return &InvalidArgumentError{Field: "AccessKeySecret", Message: "must not be blank"}
	}
	return nil
}

func buildNfsCommand(opt *NfsMountOptions) string {
	optString := opt.Options
	if strings.TrimSpace(optString) == "" {
		optString = DefaultNfsOptions
	}
	source := opt.Endpoint + ":" + opt.NasPath
	core := fmt.Sprintf(
		"mkdir -p %s && mount -t nfs -o %s %s %s",
		shQuote(opt.MountPoint),
		shQuote(optString),
		shQuote(source),
		shQuote(opt.MountPoint),
	)
	return prependInstallation(opt.Installation, core)
}

func buildOssfs1Plan(opt *OssfsMountOptions) ossfs1Plan {
	passwd := fmt.Sprintf("%s:%s:%s", opt.Bucket, opt.AccessKeyID, opt.AccessKeySecret)
	if strings.TrimSpace(opt.SecurityToken) != "" {
		// STS mode: bucket:accessKeyID:accessKeySecret:securityToken
		passwd = fmt.Sprintf("%s:%s", passwd, opt.SecurityToken)
	}

	bucketArg := opt.Bucket
	if strings.TrimSpace(opt.BucketDirectory) != "" {
		bucketArg = fmt.Sprintf("%s:/%s", opt.Bucket, opt.BucketDirectory)
	}

	var optionFlags strings.Builder
	for _, o := range opt.Options {
		optionFlags.WriteString(" -o")
		optionFlags.WriteString(shQuote(o))
	}

	// Upload the password file via the filesystem API so credentials never
	// appear in the shell command (which execd logs). Use a unique per-call
	// path under /tmp so concurrent ossfs 1.x mounts do not race on the same
	// file. Cleanup is guaranteed by capturing the ossfs exit code and
	// running rm unconditionally.
	passwdPath := ossfs1PasswdPathPrefix + randomHex(16)
	quotedPasswdPath := shQuote(passwdPath)
	core := fmt.Sprintf(
		"ossfs --version && "+
			"mkdir -p %s && "+
			"( ossfs %s %s -ourl=%s -opasswd_file=%s%s ); "+
			"__rc=$?; rm -f %s; exit $__rc",
		shQuote(opt.MountPoint),
		shQuote(bucketArg),
		shQuote(opt.MountPoint),
		shQuote(opt.Endpoint),
		quotedPasswdPath,
		optionFlags.String(),
		quotedPasswdPath,
	)
	return ossfs1Plan{
		PasswdPath:    passwdPath,
		PasswdContent: passwd,
		Command:       prependInstallation(opt.Installation, core),
	}
}

// buildOssfs1PasswdEntry converts an ossfs1Plan into the UploadFileEntry to
// deliver the password file to the sandbox.
func buildOssfs1PasswdEntry(plan ossfs1Plan) UploadFileEntry {
	return UploadFileEntry{
		File: strings.NewReader(plan.PasswdContent),
		Options: UploadFileOptions{
			FileName: filepath.Base(plan.PasswdPath),
			Metadata: FileMetadata{Path: plan.PasswdPath, Mode: 600},
		},
	}
}

func buildOssfs2Plan(opt *OssfsMountOptions) ossfs2Plan {
	var conf strings.Builder
	fmt.Fprintf(&conf, "--oss_endpoint=%s\n", opt.Endpoint)
	fmt.Fprintf(&conf, "--oss_bucket=%s\n", opt.Bucket)
	if strings.TrimSpace(opt.BucketDirectory) != "" {
		prefix := strings.TrimRight(opt.BucketDirectory, "/") + "/"
		fmt.Fprintf(&conf, "--oss_bucket_prefix=%s\n", prefix)
	}
	for _, o := range opt.Options {
		fmt.Fprintf(&conf, "--%s\n", o)
	}

	// Credentials go into a separate env file so they never appear in the
	// shell command text (which execd logs). ossfs2 reads them from the
	// process environment; sourcing the file with `set -a` marks each
	// assignment for export.
	var env strings.Builder
	fmt.Fprintf(&env, "OSS_ACCESS_KEY_ID=%s\n", opt.AccessKeyID)
	fmt.Fprintf(&env, "OSS_ACCESS_KEY_SECRET=%s\n", opt.AccessKeySecret)
	if strings.TrimSpace(opt.SecurityToken) != "" {
		fmt.Fprintf(&env, "OSS_SESSION_TOKEN=%s\n", opt.SecurityToken)
	}

	confPath := "/tmp/opensandbox-ossfs-" + randomHex(16) + ".conf"
	envPath := "/tmp/opensandbox-ossfsenv-" + randomHex(16)
	quotedConfPath := shQuote(confPath)
	quotedEnvPath := shQuote(envPath)

	// Always remove both files after the mount attempt, even on failure, so
	// repeated mounts do not accumulate credential-adjacent files in /tmp.
	// The subshell preserves the ossfs2 exit code via __rc.
	core := fmt.Sprintf(
		"ossfs2 --version && "+
			"mkdir -p %s && "+
			"set -a && . %s && set +a && "+
			"( ossfs2 mount %s -c %s ); "+
			"__rc=$?; rm -f %s %s; exit $__rc",
		shQuote(opt.MountPoint),
		quotedEnvPath,
		shQuote(opt.MountPoint),
		quotedConfPath,
		quotedConfPath,
		quotedEnvPath,
	)
	return ossfs2Plan{
		ConfPath:    confPath,
		ConfContent: conf.String(),
		EnvPath:     envPath,
		EnvContent:  env.String(),
		Command:     prependInstallation(opt.Installation, core),
	}
}

// buildOssfs2Entries converts an ossfs2Plan into the UploadFileEntry list to
// deliver the conf and env files to the sandbox.
func buildOssfs2Entries(plan ossfs2Plan) []UploadFileEntry {
	return []UploadFileEntry{
		{
			File: strings.NewReader(plan.ConfContent),
			Options: UploadFileOptions{
				FileName: filepath.Base(plan.ConfPath),
				Metadata: FileMetadata{Path: plan.ConfPath, Mode: 600},
			},
		},
		{
			File: strings.NewReader(plan.EnvContent),
			Options: UploadFileOptions{
				FileName: filepath.Base(plan.EnvPath),
				Metadata: FileMetadata{Path: plan.EnvPath, Mode: 600},
			},
		},
	}
}

func buildUmountCommand(mountPoint string) (string, error) {
	if strings.TrimSpace(mountPoint) == "" {
		return "", &InvalidArgumentError{Field: "mountPoint", Message: "must not be blank"}
	}
	return "umount " + shQuote(mountPoint), nil
}

func selectOssfsVersion(opt *OssfsMountOptions) OssfsVersion {
	if opt.Version == "" {
		return OssfsVersion10
	}
	return opt.Version
}

func ensureMountSuccess(execution *Execution, failurePrefix string) error {
	if execution == nil {
		return &MountFailedError{Message: failurePrefix + ": nil execution result"}
	}
	failed := execution.Error != nil || (execution.ExitCode != nil && *execution.ExitCode != 0)
	if !failed {
		return nil
	}
	parts := make([]string, 0, 2)
	if execution.Error != nil {
		parts = append(parts, fmt.Sprintf("[%s] %s", execution.Error.Name, execution.Error.Value))
	}
	if len(execution.Stderr) > 0 {
		var sb strings.Builder
		for i, m := range execution.Stderr {
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(m.Text)
		}
		parts = append(parts, "stderr="+sb.String())
	}
	msg := failurePrefix
	if len(parts) > 0 {
		msg = fmt.Sprintf("%s: %s", failurePrefix, strings.Join(parts, " | "))
	}
	return &MountFailedError{Message: msg, Execution: execution}
}

func prependInstallation(installation string, core string) string {
	if strings.TrimSpace(installation) == "" {
		return core
	}
	return installation + " && " + core
}

// shQuote quotes value for POSIX shell single-quoted context.
// Any embedded single quote is closed, escaped with a backslash, and reopened,
// producing the standard four-character replacement sequence. The result is
// always safe to embed as one argument to an "sh -c" string. Unlike Python's
// shlex.quote, this always adds quotes for consistent behavior across
// languages.
func shQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read should not fail on any supported platform; fall back
		// to a fixed marker so we still produce a well-formed path in the
		// impossible failure case.
		return "randfailed"
	}
	return hex.EncodeToString(buf)
}
