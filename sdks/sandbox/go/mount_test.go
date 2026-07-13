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

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// -------- shQuote --------

func TestShQuote_QuotesAndEscapesSingleQuotes(t *testing.T) {
	if got := shQuote("/mnt/nas"); got != "'/mnt/nas'" {
		t.Errorf("simple path: got %q", got)
	}
	// Embedded single quote: `wei'rd` -> `'wei'\''rd'`
	if got := shQuote("/mnt/wei'rd"); got != "'/mnt/wei'\\''rd'" {
		t.Errorf("quoted path: got %q", got)
	}
}

// -------- NFS builder --------

func TestBuildNfsCommand_UsesDefaultOptionsAndQuotesPaths(t *testing.T) {
	cmd := buildNfsCommand(&NfsMountOptions{
		Endpoint:   "nas-server.example.com",
		NasPath:    "/share",
		MountPoint: "/mnt/nas",
	})
	if !strings.Contains(cmd, "mkdir -p '/mnt/nas'") {
		t.Errorf("expected mkdir -p, got: %s", cmd)
	}
	if !strings.Contains(cmd, "mount -t nfs -o '"+DefaultNfsOptions+"' 'nas-server.example.com:/share' '/mnt/nas'") {
		t.Errorf("expected full quoted mount command, got: %s", cmd)
	}
}

func TestBuildNfsCommand_HonorsCustomOptionsAndInstallation(t *testing.T) {
	cmd := buildNfsCommand(&NfsMountOptions{
		Endpoint:     "nas",
		NasPath:      "/",
		MountPoint:   "/mnt/x",
		Options:      "vers=4,proto=tcp",
		Installation: "apt-get install -y nfs-common",
	})
	if !strings.HasPrefix(cmd, "apt-get install -y nfs-common && ") {
		t.Errorf("expected installation prefix, got: %s", cmd)
	}
	if !strings.Contains(cmd, "'vers=4,proto=tcp'") {
		t.Errorf("expected custom options, got: %s", cmd)
	}
	if strings.Contains(cmd, DefaultNfsOptions) {
		t.Errorf("should not fall back to default when custom opts provided, got: %s", cmd)
	}
}

func TestValidateNfs_RejectsBlankFields(t *testing.T) {
	cases := []*NfsMountOptions{
		{Endpoint: "  ", NasPath: "/", MountPoint: "/mnt/x"},
		{Endpoint: "nas", NasPath: "", MountPoint: "/mnt/x"},
		{Endpoint: "nas", NasPath: "/", MountPoint: " "},
	}
	for i, opt := range cases {
		var e *InvalidArgumentError
		if err := validateNfs(opt); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		} else if !errors.As(err, &e) {
			t.Errorf("case %d: expected *InvalidArgumentError, got %T", i, err)
		}
	}
}

// -------- ossfs 1.x plan --------

// assertNoCredentialsInCommand guards the P1 fix: shell text is logged by
// execd, so raw AK/SK/token strings must never appear in it.
func assertNoCredentialsInCommand(t *testing.T, cmd string) {
	t.Helper()
	for _, bad := range []string{
		"printf %s",
		"install -m 600 /dev/null",
		"export OSS_ACCESS_KEY_ID=",
		"export OSS_ACCESS_KEY_SECRET=",
		"export OSS_SESSION_TOKEN=",
	} {
		if strings.Contains(cmd, bad) {
			t.Errorf("credentials must not appear in command via %q, got: %s", bad, cmd)
		}
	}
}

func TestBuildOssfs1Plan_UploadsPasswdFileAndCleansUp(t *testing.T) {
	plan := buildOssfs1Plan(&OssfsMountOptions{
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "my-bucket",
		MountPoint:      "/mnt/oss",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	})

	if plan.PasswdContent != "my-bucket:AK:SK" {
		t.Errorf("passwd content mismatch: %q", plan.PasswdContent)
	}
	if !strings.HasPrefix(plan.PasswdPath, ossfs1PasswdPathPrefix) {
		t.Errorf("passwd path must start with %q, got: %q", ossfs1PasswdPathPrefix, plan.PasswdPath)
	}
	quotedPasswdPath := "'" + plan.PasswdPath + "'"

	cmd := plan.Command
	checks := []string{
		"__rc=$?; rm -f " + quotedPasswdPath + "; exit $__rc",
		"ossfs 'my-bucket' '/mnt/oss'",
		"-ourl='https://oss-cn-hangzhou.aliyuncs.com'",
		"-opasswd_file=" + quotedPasswdPath,
	}
	for _, want := range checks {
		if !strings.Contains(cmd, want) {
			t.Errorf("expected %q, got: %s", want, cmd)
		}
	}
	assertNoCredentialsInCommand(t, cmd)
}

func TestBuildOssfs1PasswdEntry_UsesMode600(t *testing.T) {
	plan := buildOssfs1Plan(&OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	})
	entry := buildOssfs1PasswdEntry(plan)
	if entry.Options.Metadata.Path != plan.PasswdPath {
		t.Errorf("entry path mismatch: %q vs %q", entry.Options.Metadata.Path, plan.PasswdPath)
	}
	if entry.Options.Metadata.Mode != 600 {
		t.Errorf("expected mode 600, got %d", entry.Options.Metadata.Mode)
	}
}

func TestBuildOssfs1Plan_UsesDistinctPasswdPathPerCall(t *testing.T) {
	opts := &OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	}
	a := buildOssfs1Plan(opts).PasswdPath
	b := buildOssfs1Plan(opts).PasswdPath
	if a == "" || b == "" {
		t.Fatalf("expected passwd paths, got %q and %q", a, b)
	}
	if a == b {
		t.Errorf("concurrent-safe ossfs1 mounts must not share the same passwd file, got %q twice", a)
	}
}

func TestBuildOssfs1Plan_SupportsSTSTokenBucketDirectoryAndOptions(t *testing.T) {
	plan := buildOssfs1Plan(&OssfsMountOptions{
		Endpoint:        "https://oss.example.com",
		Bucket:          "b",
		BucketDirectory: "subdir",
		MountPoint:      "/mnt/oss",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
		Options:         []string{"use_cache=/tmp/ossfs", "allow_other"},
	})
	if plan.PasswdContent != "b:AK:SK:TOKEN" {
		t.Errorf("STS passwd content mismatch: %q", plan.PasswdContent)
	}
	cmd := plan.Command
	for _, want := range []string{
		"ossfs 'b:/subdir' '/mnt/oss'",
		"-o'use_cache=/tmp/ossfs'",
		"-o'allow_other'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("expected %q, got: %s", want, cmd)
		}
	}
	assertNoCredentialsInCommand(t, cmd)
}

func TestValidateOssfs_RejectsBlankCredentials(t *testing.T) {
	base := OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
	}
	tries := []func(o *OssfsMountOptions){
		func(o *OssfsMountOptions) { o.AccessKeyID = " " },
		func(o *OssfsMountOptions) { o.AccessKeySecret = "" },
		func(o *OssfsMountOptions) { o.Bucket = "" },
	}
	for i, mut := range tries {
		opt := base
		mut(&opt)
		var e *InvalidArgumentError
		if err := validateOssfs(&opt); err == nil {
			t.Errorf("case %d: expected error", i)
		} else if !errors.As(err, &e) {
			t.Errorf("case %d: expected *InvalidArgumentError, got %T", i, err)
		}
	}
}

// -------- ossfs 2.x plan --------

func TestBuildOssfs2Plan_UploadsConfAndEnvFilesAndCleansUp(t *testing.T) {
	plan := buildOssfs2Plan(&OssfsMountOptions{
		Endpoint:        "https://oss.example.com",
		Bucket:          "b",
		MountPoint:      "/mnt/oss2",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		Options:         []string{"cache_dir=/tmp/ossfs2"},
		Version:         OssfsVersion20,
	})
	if !strings.HasPrefix(plan.ConfPath, "/tmp/opensandbox-ossfs-") || !strings.HasSuffix(plan.ConfPath, ".conf") {
		t.Errorf("unexpected conf path: %s", plan.ConfPath)
	}
	if !strings.HasPrefix(plan.EnvPath, "/tmp/opensandbox-ossfsenv-") {
		t.Errorf("unexpected env path: %s", plan.EnvPath)
	}
	for _, want := range []string{
		"--oss_endpoint=https://oss.example.com\n",
		"--oss_bucket=b\n",
		"--cache_dir=/tmp/ossfs2\n",
	} {
		if !strings.Contains(plan.ConfContent, want) {
			t.Errorf("conf missing %q, got:\n%s", want, plan.ConfContent)
		}
	}
	for _, want := range []string{
		"OSS_ACCESS_KEY_ID=AK\n",
		"OSS_ACCESS_KEY_SECRET=SK\n",
	} {
		if !strings.Contains(plan.EnvContent, want) {
			t.Errorf("env missing %q, got:\n%s", want, plan.EnvContent)
		}
	}
	for _, want := range []string{
		"ossfs2 --version",
		"set -a && . '" + plan.EnvPath + "' && set +a",
		"( ossfs2 mount '/mnt/oss2' -c '" + plan.ConfPath + "' )",
		"__rc=$?; rm -f '" + plan.ConfPath + "' '" + plan.EnvPath + "'; exit $__rc",
	} {
		if !strings.Contains(plan.Command, want) {
			t.Errorf("command missing %q, got: %s", want, plan.Command)
		}
	}
	assertNoCredentialsInCommand(t, plan.Command)
}

func TestBuildOssfs2Entries_ReturnsConfAndEnvWithMode600(t *testing.T) {
	plan := buildOssfs2Plan(&OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		Version:         OssfsVersion20,
	})
	entries := buildOssfs2Entries(plan)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (conf + env), got %d", len(entries))
	}
	byPath := make(map[string]UploadFileEntry, 2)
	for _, e := range entries {
		byPath[e.Options.Metadata.Path] = e
	}
	confEntry, ok := byPath[plan.ConfPath]
	if !ok {
		t.Fatalf("missing conf entry, got paths: %v", byPath)
	}
	envEntry, ok := byPath[plan.EnvPath]
	if !ok {
		t.Fatalf("missing env entry, got paths: %v", byPath)
	}
	if confEntry.Options.Metadata.Mode != 600 || envEntry.Options.Metadata.Mode != 600 {
		t.Errorf("expected mode 600 on both, got conf=%d env=%d",
			confEntry.Options.Metadata.Mode, envEntry.Options.Metadata.Mode)
	}
	// Sanity-check body content is streamed correctly.
	confBody, _ := io.ReadAll(confEntry.File)
	envBody, _ := io.ReadAll(envEntry.File)
	if string(confBody) != plan.ConfContent {
		t.Errorf("conf body mismatch")
	}
	if string(envBody) != plan.EnvContent {
		t.Errorf("env body mismatch")
	}
}

func TestBuildOssfs2Plan_EncodesBucketDirectoryAsPrefix(t *testing.T) {
	plan := buildOssfs2Plan(&OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		BucketDirectory: "sub/dir",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		Version:         OssfsVersion20,
	})
	if !strings.Contains(plan.ConfContent, "--oss_bucket_prefix=sub/dir/\n") {
		t.Errorf("conf missing bucket prefix, got:\n%s", plan.ConfContent)
	}
}

func TestBuildOssfs2Plan_EnvIncludesSessionTokenWhenSet(t *testing.T) {
	plan := buildOssfs2Plan(&OssfsMountOptions{
		Endpoint:        "e",
		Bucket:          "b",
		MountPoint:      "/mnt/x",
		AccessKeyID:     "AK",
		AccessKeySecret: "SK",
		SecurityToken:   "TOKEN",
		Version:         OssfsVersion20,
	})
	if !strings.Contains(plan.EnvContent, "OSS_SESSION_TOKEN=TOKEN\n") {
		t.Errorf("env missing STS token, got: %s", plan.EnvContent)
	}
	assertNoCredentialsInCommand(t, plan.Command)
}

func TestSelectOssfsVersion_DefaultsToV10(t *testing.T) {
	if v := selectOssfsVersion(&OssfsMountOptions{}); v != OssfsVersion10 {
		t.Errorf("expected default ossfs 1.0, got %q", v)
	}
	if v := selectOssfsVersion(&OssfsMountOptions{Version: OssfsVersion20}); v != OssfsVersion20 {
		t.Errorf("expected ossfs 2.0 when set, got %q", v)
	}
}

// -------- umount --------

func TestBuildUmountCommand(t *testing.T) {
	cmd, err := buildUmountCommand("/mnt/nas")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "umount '/mnt/nas'" {
		t.Errorf("got: %s", cmd)
	}
	var e *InvalidArgumentError
	if _, err := buildUmountCommand("   "); err == nil {
		t.Errorf("expected error for blank")
	} else if !errors.As(err, &e) {
		t.Errorf("expected *InvalidArgumentError, got %T", err)
	}
}

// -------- ensureMountSuccess --------

func TestEnsureMountSuccess_PassesOnZeroExit(t *testing.T) {
	zero := 0
	if err := ensureMountSuccess(&Execution{ExitCode: &zero}, "prefix"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureMountSuccess_ReturnsMountFailedErrorOnErrorField(t *testing.T) {
	one := 1
	exec := &Execution{
		ExitCode: &one,
		Error:    &ExecutionError{Name: "MountError", Value: "denied"},
		Stderr:   []OutputMessage{{Text: "stderr text"}},
	}
	err := ensureMountSuccess(exec, "NAS mount failure")
	var mfe *MountFailedError
	if !errors.As(err, &mfe) {
		t.Fatalf("expected *MountFailedError, got %T (%v)", err, err)
	}
	if mfe.Execution != exec {
		t.Errorf("expected execution attached to error")
	}
	for _, want := range []string{"NAS mount failure", "MountError", "stderr=stderr text"} {
		if !strings.Contains(mfe.Message, want) {
			t.Errorf("message missing %q, got: %s", want, mfe.Message)
		}
	}
}

func TestEnsureMountSuccess_ReturnsMountFailedErrorOnNonZeroExit(t *testing.T) {
	two := 2
	err := ensureMountSuccess(&Execution{ExitCode: &two}, "prefix")
	var mfe *MountFailedError
	if !errors.As(err, &mfe) {
		t.Fatalf("expected *MountFailedError, got %T", err)
	}
}

// -------- Sandbox.Mount dispatch --------

func TestSandbox_Mount_RejectsNilOptions(t *testing.T) {
	sb := &Sandbox{}
	ctx := context.Background()
	if _, err := sb.Mount(ctx, (*NfsMountOptions)(nil)); err == nil {
		t.Errorf("expected error for nil NfsMountOptions")
	}
	if _, err := sb.Mount(ctx, (*OssfsMountOptions)(nil)); err == nil {
		t.Errorf("expected error for nil OssfsMountOptions")
	}
}

func TestSandbox_Mount_RejectsUnknownOptionsType(t *testing.T) {
	sb := &Sandbox{}
	// Compile-time: MountOptions is a sealed interface, so we cannot pass a
	// plain string here. Use a private stub that satisfies the interface with
	// an unexpected concrete type.
	if _, err := sb.Mount(context.Background(), unknownMountOptions{}); err == nil {
		t.Errorf("expected error for unknown option type")
	}
}

type unknownMountOptions struct{}

func (unknownMountOptions) isMountOptions() {}
