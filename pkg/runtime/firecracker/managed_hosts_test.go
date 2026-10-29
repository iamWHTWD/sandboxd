// Copyright (c) 2026 Ant Group Corporation.
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

package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inclusionAI/sandboxd/config"
	runtimecore "github.com/inclusionAI/sandboxd/pkg/runtime"
)

func firecrackerTestManagedHosts() *runtimecore.ManagedHostsState {
	return &runtimecore.ManagedHostsState{
		Version: runtimecore.ManagedHostsVersion,
		Layout:  runtimecore.HostsLayoutPrivateRootfs,
		Runtime: config.RuntimeNameFirecracker,
	}
}

func TestFirecrackerManagedHostsStorage(t *testing.T) {
	spec := &runtimecore.Spec{
		Root:    &runtimecore.Root{Path: fakeEROFSImage(t, "root.erofs")},
		Process: &runtimecore.Process{Args: []string{"/bin/sh"}},
	}
	start := runtimecore.StartConfig{
		Network:      firecrackerTestNetwork(),
		ManagedHosts: firecrackerTestManagedHosts(),
		InitialFiles: []runtimecore.InitialFile{{Path: "/etc/hosts", Content: []byte("127.0.0.1 localhost\n"), Mode: 0644}},
	}
	plan, err := prepareFirecrackerStorage(spec, start, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.configure.Files) != 1 || plan.configure.Files[0].Readonly || plan.configure.Files[0].Mode != 0644 || plan.configure.Files[0].Target != "/etc/hosts" {
		t.Fatalf("unexpected hosts injection: %+v", plan.configure.Files)
	}
	start.InitialFiles[0].Content[0] = '9'
	if plan.configure.Files[0].Content[0] != '1' {
		t.Fatal("configure content aliases caller-owned buffer")
	}
	// A restore plans block devices without replaying initialization files.
	start.CheckpointDir, start.InitialFiles = t.TempDir(), nil
	plan, err = prepareFirecrackerStorage(spec, start, false)
	if err != nil || len(plan.configure.Files) != 0 {
		t.Fatalf("restore initial files = %+v, err = %v", plan, err)
	}
}

func TestFirecrackerManagedHostsLayoutValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*runtimecore.Spec, *runtimecore.StartConfig, *[]runtimecore.Mount, *firecrackerExtraConfig)
		wantErr bool
	}{
		{name: "fresh private"},
		{name: "missing initialization", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.InitialFiles = nil
		}, wantErr: true},
		{name: "no metadata", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.ManagedHosts = nil
		}, wantErr: true},
		{name: "readonly final root", change: func(s *runtimecore.Spec, _ *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			s.Root.Readonly = true
		}, wantErr: true},
		{name: "parent mount", change: func(_ *runtimecore.Spec, _ *runtimecore.StartConfig, m *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			*m = []runtimecore.Mount{{Destination: "/etc", Options: []string{"ro"}}}
		}, wantErr: true},
		{name: "direct mount", change: func(_ *runtimecore.Spec, _ *runtimecore.StartConfig, m *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			*m = []runtimecore.Mount{{Destination: "/etc/hosts"}}
		}, wantErr: true},
		{name: "native parent mount", change: func(_ *runtimecore.Spec, _ *runtimecore.StartConfig, _ *[]runtimecore.Mount, e *firecrackerExtraConfig) {
			e.NativeWritableMounts = []firecrackerNativeWritableMount{{Target: "/etc"}}
		}, wantErr: true},
		{name: "restore replay", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.CheckpointDir = "/checkpoint"
		}, wantErr: true},
		{name: "invalid target", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.InitialFiles[0].Path = "/root/key"
		}, wantErr: true},
		{name: "invalid mode", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.InitialFiles[0].Mode = 0666
		}, wantErr: true},
		{name: "oversize", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.InitialFiles[0].Content = make([]byte, runtimecore.ManagedHostsContentLimit+1)
		}, wantErr: true},
		{name: "legacy readonly mount", change: func(s *runtimecore.Spec, c *runtimecore.StartConfig, m *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			s.Root.Readonly = true
			c.ManagedHosts.RootReadonly = true
			c.ManagedHosts.Layout = runtimecore.HostsLayoutLegacy
			c.InitialFiles = nil
			*m = []runtimecore.Mount{{Destination: "/etc/hosts", Options: []string{"ro"}}}
		}},
		{name: "external parent", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, m *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.ManagedHosts.Layout = runtimecore.HostsLayoutExternal
			c.InitialFiles = nil
			*m = []runtimecore.Mount{{Destination: "/etc", Options: []string{"rw"}}}
		}},
		{name: "external missing mount", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, _ *firecrackerExtraConfig) {
			c.ManagedHosts.Layout = runtimecore.HostsLayoutExternal
			c.InitialFiles = nil
		}, wantErr: true},
		{name: "external native parent", change: func(_ *runtimecore.Spec, c *runtimecore.StartConfig, _ *[]runtimecore.Mount, e *firecrackerExtraConfig) {
			c.ManagedHosts.Layout = runtimecore.HostsLayoutExternal
			c.InitialFiles = nil
			e.NativeWritableMounts = []firecrackerNativeWritableMount{{Target: "/etc"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &runtimecore.Spec{Root: &runtimecore.Root{}}
			start := runtimecore.StartConfig{ManagedHosts: firecrackerTestManagedHosts(), InitialFiles: []runtimecore.InitialFile{{Path: "/etc/hosts", Mode: 0644}}}
			var mounts []runtimecore.Mount
			var extra firecrackerExtraConfig
			if tc.change != nil {
				tc.change(spec, &start, &mounts, &extra)
			}
			if err := validateFirecrackerHostsLayout(spec, start, mounts, extra); (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestFirecrackerInitialHostsShareInjectionLimit(t *testing.T) {
	spec := &runtimecore.Spec{Root: &runtimecore.Root{Path: fakeEROFSImage(t, "root.erofs")}, Process: &runtimecore.Process{Args: []string{"/bin/sh"}}}
	file := filepath.Join(t.TempDir(), "metadata")
	if err := os.WriteFile(file, make([]byte, firecrackerMaxInjectedFile), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/a", "/b", "/c", "/d"} {
		spec.Mounts = append(spec.Mounts, runtimecore.Mount{Type: "bind", Source: file, Destination: target, Options: []string{"ro"}})
	}
	start := runtimecore.StartConfig{Network: firecrackerTestNetwork(), ManagedHosts: firecrackerTestManagedHosts(), InitialFiles: []runtimecore.InitialFile{{Path: "/etc/hosts", Mode: 0644, Content: []byte("x")}}}
	if _, err := prepareFirecrackerStorage(spec, start, false); err == nil || !strings.Contains(err.Error(), "injected files exceed") {
		t.Fatalf("aggregate limit error = %v", err)
	}
}

func sealManagedHostsFixture(t *testing.T, dir string) firecrackerCheckpointFiles {
	t.Helper()
	files := sealArtifactFixture(t, dir)
	files.ManagedHosts = filepath.Join(dir, runtimecore.ManagedHostsStateFile)
	if err := runtimecore.WriteManagedHostsState(dir, firecrackerTestManagedHosts()); err != nil {
		t.Fatal(err)
	}
	manifest := &firecrackerCheckpointManifest{SnapshotType: firecrackerSnapshotTypeFull, ManagedHosts: runtimecore.ManagedHostsStateFile}
	if err := finalizeFirecrackerCheckpointV2(context.Background(), files, manifest); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestFirecrackerManagedHostsCheckpointIntegrity(t *testing.T) {
	for _, mutation := range []string{"none", "missing", "invalid json", "changed", "symlink", "no declaration", "wrong declaration", "no digest", "invalid digest"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			files := sealManagedHostsFixture(t, dir)
			manifest, err := readFirecrackerCheckpointManifest(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing":
				err = os.Remove(files.ManagedHosts)
			case "invalid json":
				err = os.WriteFile(files.ManagedHosts, []byte("null"), 0600)
			case "changed":
				state := firecrackerTestManagedHosts()
				state.Layout = runtimecore.HostsLayoutExternal
				err = runtimecore.WriteManagedHostsState(dir, state)
			case "symlink":
				if err = os.Rename(files.ManagedHosts, files.ManagedHosts+".target"); err == nil {
					err = os.Symlink(files.ManagedHosts+".target", files.ManagedHosts)
				}
			case "no declaration":
				manifest.ManagedHosts = ""
			case "wrong declaration":
				manifest.ManagedHosts = "../managed-hosts.json"
			case "no digest":
				delete(manifest.Digests, runtimecore.ManagedHostsStateFile)
			case "invalid digest":
				manifest.Digests[runtimecore.ManagedHostsStateFile] = ""
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, firecrackerCheckpointManifestName), data, 0600); err != nil {
				t.Fatal(err)
			}
			artifact, err := openFirecrackerCheckpoint(dir)
			if mutation == "none" {
				if err != nil || artifact.ManagedHosts == nil || *artifact.ManagedHosts != *firecrackerTestManagedHosts() {
					t.Fatalf("artifact = %+v, err = %v", artifact, err)
				}
			} else if err == nil {
				t.Fatal("accepted inconsistent managed hosts artifact")
			}
		})
	}
}

func TestFirecrackerManagedHostsLegacyAndUnsealedArtifacts(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		dir := t.TempDir()
		if legacy {
			if err := os.WriteFile(filepath.Join(dir, checkpointImageName), []byte("archive"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			files := sealArtifactFixture(t, dir)
			if err := finalizeFirecrackerCheckpointV2(context.Background(), files, &firecrackerCheckpointManifest{SnapshotType: firecrackerSnapshotTypeFull}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := openFirecrackerCheckpoint(dir); err != nil {
			t.Fatal(err)
		}
		if err := runtimecore.WriteManagedHostsState(dir, firecrackerTestManagedHosts()); err != nil {
			t.Fatal(err)
		}
		if _, err := openFirecrackerCheckpoint(dir); err == nil {
			t.Fatal("legacy artifact accepted undeclared hosts sidecar")
		}
	}
	dir := t.TempDir()
	files := sealArtifactFixture(t, dir)
	files.ManagedHosts = filepath.Join(dir, runtimecore.ManagedHostsStateFile)
	if err := finalizeFirecrackerCheckpointV2(context.Background(), files, &firecrackerCheckpointManifest{SnapshotType: firecrackerSnapshotTypeFull, ManagedHosts: runtimecore.ManagedHostsStateFile}); err == nil {
		t.Fatal("sealed missing sidecar")
	}
	if _, err := os.Stat(filepath.Join(dir, firecrackerCheckpointManifestName)); !os.IsNotExist(err) {
		t.Fatalf("published failed manifest: %v", err)
	}
	if err := runtimecore.WriteManagedHostsState(dir, firecrackerTestManagedHosts()); err != nil {
		t.Fatal(err)
	}
	discardUnsealedFirecrackerCheckpoint(files)
	if _, err := os.Stat(files.ManagedHosts); !os.IsNotExist(err) {
		t.Fatalf("sidecar left after failed checkpoint: %v", err)
	}
}

func TestFirecrackerManagedHostsRestoreAndPersistence(t *testing.T) {
	handler := &Handler{runtimeName: config.RuntimeNameFirecracker}
	saved := firecrackerTestManagedHosts()
	artifact := &firecrackerCheckpointArtifact{ManagedHosts: saved}
	start := runtimecore.StartConfig{CheckpointDir: "/checkpoint"}
	if err := handler.restoreManagedHosts(&start, artifact); err != nil {
		t.Fatal(err)
	}
	if start.ManagedHosts == saved || *start.ManagedHosts != *saved {
		t.Fatal("restored metadata was not independently retained")
	}
	data, err := json.Marshal(firecrackerPersistedState{ManagedHosts: start.ManagedHosts})
	if err != nil {
		t.Fatal(err)
	}
	var state firecrackerPersistedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := runtimecore.WriteManagedHostsState(dir, state.ManagedHosts); err != nil {
		t.Fatal(err)
	}
	reloaded, err := runtimecore.ReadManagedHostsState(dir)
	if err != nil || *reloaded != *saved {
		t.Fatalf("persisted hosts = %+v, err = %v", reloaded, err)
	}
	start.ManagedHosts.Layout = runtimecore.HostsLayoutExternal
	if err := handler.restoreManagedHosts(&start, artifact); err == nil {
		t.Fatal("restore accepted changed layout")
	}
	start.ManagedHosts = nil
	start.InitialFiles = []runtimecore.InitialFile{{Path: "/etc/hosts", Mode: 0644}}
	if err := handler.restoreManagedHosts(&start, artifact); err == nil {
		t.Fatal("restore accepted initial files")
	}
	pvm := &Handler{runtimeName: config.RuntimeNameFirecrackerPVM}
	start.InitialFiles = nil
	if err := pvm.restoreManagedHosts(&start, artifact); err == nil {
		t.Fatal("PVM accepted standard runtime hosts metadata")
	}
	if err := pvm.restoreManagedHosts(&start, &firecrackerCheckpointArtifact{}); err != nil {
		t.Fatalf("legacy PVM changed: %v", err)
	}
}

func TestFirecrackerRestoreOverlapPreservesInput(t *testing.T) {
	for _, location := range []string{"bundle", "storage", "runtime"} {
		t.Run(location, func(t *testing.T) {
			handler := &Handler{sandboxRoot: t.TempDir(), storageRoot: t.TempDir(), runtimeRoot: t.TempDir()}
			id := "sbox-protected-input"
			checkpoint := filepath.Join(handler.sandboxRoot, id)
			if location == "storage" {
				checkpoint = filepath.Join(handler.storageRoot, id)
			}
			if location == "runtime" {
				checkpoint = handler.runtimeDirectory(id)
			}
			if err := os.MkdirAll(checkpoint, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(checkpoint, runtimecore.ManagedHostsStateFile)
			before := []byte("caller-owned input")
			if err := os.WriteFile(file, before, 0600); err != nil {
				t.Fatal(err)
			}
			err := handler.Restore(context.Background(), runtimecore.StartConfig{ID: id, CheckpointDir: checkpoint})
			if err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("overlapping restore accepted: %v", err)
			}
			after, err := os.ReadFile(file)
			if err != nil || string(after) != string(before) {
				t.Fatalf("input changed: %q %v", after, err)
			}
		})
	}
}

func TestFirecrackerCheckpointRejectsSandboxOwnedOutput(t *testing.T) {
	for _, location := range []string{"bundle", "storage", "runtime"} {
		t.Run(location, func(t *testing.T) {
			handler := &Handler{sandboxRoot: t.TempDir(), storageRoot: t.TempDir(), runtimeRoot: t.TempDir()}
			id := "sbox-protected-output"
			owned := filepath.Join(handler.sandboxRoot, id)
			if location == "storage" {
				owned = filepath.Join(handler.storageRoot, id)
			}
			if location == "runtime" {
				owned = handler.runtimeDirectory(id)
			}
			if err := os.MkdirAll(owned, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(owned, "source-state")
			if err := os.WriteFile(sentinel, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(owned, "checkpoint")
			err := handler.Checkpoint(context.Background(), runtimecore.CheckpointConfig{ID: id, Directory: output})
			if err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("sandbox-owned output was not rejected before runtime access: %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("output was created: %v", err)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "preserved" {
				t.Fatalf("source changed: %q %v", data, err)
			}
			if err := handler.validateCheckpointDirectorySeparation(t.TempDir(), id); err != nil {
				t.Fatalf("independent output rejected: %v", err)
			}
		})
	}
}
