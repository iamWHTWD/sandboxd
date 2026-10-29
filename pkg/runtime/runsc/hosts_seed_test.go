// Copyright (c) 2026 Ant Group Corporation.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package runsc

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	runtimecore "github.com/inclusionAI/sandboxd/pkg/runtime"
	"golang.org/x/sys/unix"
)

func TestHostsSeedPreservesDirectoryMetadata(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.Mkdir(etc, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(etc, 0750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1234567890, 123456789)
	if err := os.Chtimes(etc, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(etc, "user.hosts-seed-test", []byte("preserved"), 0); err != nil {
		t.Fatal(err)
	}
	// A minimal default ACL remains meaningful even without named users.
	acl := make([]byte, 4+3*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range [][2]uint16{{1, 7}, {4, 5}, {32, 0}} {
		offset := 4 + i*8
		binary.LittleEndian.PutUint16(acl[offset:], entry[0])
		binary.LittleEndian.PutUint16(acl[offset+2:], entry[1])
		binary.LittleEndian.PutUint32(acl[offset+4:], ^uint32(0))
	}
	if err := unix.Setxattr(etc, "system.posix_acl_default", acl, 0); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	headers, err := h.readHostsSeedHeaders(t.TempDir(), &runtimecore.Spec{Root: &runtimecore.Root{Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(etc)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if headers[1].Mode != int64(stat.Mode&07777) || headers[1].Uid != int(stat.Uid) || headers[1].Gid != int(stat.Gid) || !headers[1].ModTime.Equal(mtime) {
		t.Fatalf("directory metadata changed: %+v, stat=%+v", headers[1], stat)
	}
	if headers[1].PAXRecords["SCHILY.xattr.user.hosts-seed-test"] != "preserved" || headers[1].PAXRecords["SCHILY.xattr.system.posix_acl_default"] != base64.RawStdEncoding.EncodeToString(acl) {
		t.Fatalf("xattrs or ACL were not preserved: %#v", headers[1].PAXRecords)
	}
	data, err := encodeHostsSeed(headers, []runtimecore.InitialFile{{Path: "/etc/hosts", Content: []byte("initial\n"), Mode: 0644}})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	for _, name := range []string{"./", "./etc/", "./etc/hosts"} {
		header, err := reader.Next()
		if err != nil || header.Name != name {
			t.Fatalf("header=%+v error=%v, want %s", header, err, name)
		}
		if name == "./etc/" && (!header.ModTime.Equal(mtime) || header.Mode != int64(stat.Mode&07777)) {
			t.Fatalf("tar lost directory metadata: %+v", header)
		}
		if name == "./etc/hosts" {
			content, err := io.ReadAll(reader)
			if err != nil || string(content) != "initial\n" || header.Mode != 0644 || header.Uid != 0 || header.Gid != 0 {
				t.Fatalf("bad hosts entry %+v content=%q err=%v", header, content, err)
			}
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("unexpected extra tar entry: %v", err)
	}
}

func TestHostsSeedRejectsUnsafeParents(t *testing.T) {
	for _, kind := range []string{"symlink", "file", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			switch kind {
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "etc")); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(filepath.Join(root, "etc"), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			headers, err := (&Handler{}).readHostsSeedHeaders(t.TempDir(), &runtimecore.Spec{Root: &runtimecore.Root{Path: root}})
			if kind != "missing" {
				if err == nil {
					t.Fatal("accepted non-directory /etc")
				}
			} else if err != nil || headers[1].Mode != 0755 || headers[1].Uid != 0 || headers[1].Gid != 0 {
				t.Fatalf("missing parent: %+v, %v", headers, err)
			}
		})
	}
}

func TestHostsXattrRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{"user.binary", []byte{255}}, {"user.empty", nil}, {"user.bad=name", []byte("ok")},
	} {
		if _, err := hostsXattrPAXValue(tc.name, tc.value); err == nil {
			t.Fatalf("silently accepted unrepresentable attribute %q", tc.name)
		}
	}
	value := []byte{0, 255, 3}
	encoded, err := hostsXattrPAXValue("system.posix_acl_access", value)
	if err != nil || encoded != base64.RawStdEncoding.EncodeToString(value) {
		t.Fatalf("ACL encoding=%q, %v", encoded, err)
	}
	if encoded, err := hostsXattrPAXValue("security.selinux", []byte("label\x00")); err != nil || encoded != "label\x00" {
		t.Fatalf("PAX xattr trailing NUL was rejected: %q %v", encoded, err)
	}
}

func TestHostsEROFSMetadataMountCleanup(t *testing.T) {
	for _, failure := range []string{"", "mount", "metadata", "unmount"} {
		t.Run(failure, func(t *testing.T) {
			bundle := t.TempDir()
			if err := os.Mkdir(filepath.Join(bundle, "rootfs"), 0755); err != nil {
				t.Fatal(err)
			}
			originalUnmount := unmountRunscNVProxyPath
			t.Cleanup(func() { unmountRunscNVProxyPath = originalUnmount })
			unmounts := 0
			unmountRunscNVProxyPath = func(target string, flags int) error {
				unmounts++
				if target != filepath.Join(bundle, hostsMetadataLowerDir) {
					t.Fatalf("unexpected unmount %s", target)
				}
				if failure == "unmount" {
					return syscall.EPERM
				}
				return nil
			}
			h := &Handler{mountEROFS: func(source, target string) error {
				if source != "/image.erofs" {
					t.Fatalf("wrong source %s", source)
				}
				if failure == "mount" {
					return errors.New("mount failed")
				}
				if failure == "metadata" {
					return os.Symlink("/outside", filepath.Join(target, "etc"))
				}
				return os.Mkdir(filepath.Join(target, "etc"), 0711)
			}}
			spec := &runtimecore.Spec{Root: &runtimecore.Root{Path: "rootfs"}, Annotations: map[string]string{runtimecore.GVisorRootfsAnnotationPrefix + "type": "erofs", runtimecore.GVisorRootfsAnnotationPrefix + "source": "/image.erofs"}}
			headers, err := h.readHostsSeedHeaders(bundle, spec)
			if (err != nil) != (failure != "") {
				t.Fatalf("headers=%v err=%v failure=%s", headers, err, failure)
			}
			if unmounts == 0 {
				t.Fatal("metadata mount cleanup was skipped")
			}
			_, statErr := os.Stat(filepath.Join(bundle, hostsMetadataLowerDir))
			if failure != "unmount" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("mount directory remains: %v", statErr)
			}
			if failure == "unmount" && (err == nil || !strings.Contains(err.Error(), "unmount")) {
				t.Fatal("unmount failure was swallowed")
			}
			if failure == "" && headers[1].Mode != 0711 {
				t.Fatalf("read OCI placeholder instead of image lower: %+v", headers[1])
			}
		})
	}
}

func TestHostsMetadataCleanupRejectsSymlink(t *testing.T) {
	bundle, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(bundle, hostsMetadataLowerDir)); err != nil {
		t.Fatal(err)
	}
	original := unmountRunscNVProxyPath
	t.Cleanup(func() { unmountRunscNVProxyPath = original })
	unmountRunscNVProxyPath = func(string, int) error { t.Fatal("attempted unmount through a symlink"); return nil }
	if err := cleanupHostsMetadataMount(bundle); err == nil {
		t.Fatal("accepted symlinked metadata mount")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("changed symlink target: %v", err)
	}
}
