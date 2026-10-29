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

package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testManagedHostsState() *ManagedHostsState {
	return &ManagedHostsState{
		Version: ManagedHostsVersion, Layout: HostsLayoutPrivateRootfs, Runtime: "runsc",
		Seed: &RootfsSeed{File: RootfsSeedName, SHA256: strings.Repeat("a", 64), AnnotationKey: GVisorRootfsTarUpperAnnotation, AnnotationValue: RootfsSeedName},
	}
}

func TestManagedHostsStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadManagedHostsState(dir)
	if err != nil || got != nil {
		t.Fatalf("legacy: %v, %v", got, err)
	}
	want := testManagedHostsState()
	if err := WriteManagedHostsState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err = ReadManagedHostsState(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip: %#v, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, ManagedHostsStateFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("metadata permissions: %v, %v", info, err)
	}
	want.Layout = HostsLayoutExternal
	want.Seed = nil
	if err := WriteManagedHostsState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err = ReadManagedHostsState(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("replacement: %#v, %v", got, err)
	}
}

func TestManagedHostsRejectsInvalidState(t *testing.T) {
	cases := map[string]func(*ManagedHostsState){
		"version":             func(s *ManagedHostsState) { s.Version++ },
		"runtime":             func(s *ManagedHostsState) { s.Runtime = "firecracker-pvm" },
		"layout":              func(s *ManagedHostsState) { s.Layout = "unknown" },
		"readonly private":    func(s *ManagedHostsState) { s.RootReadonly = true },
		"missing seed":        func(s *ManagedHostsState) { s.Seed = nil },
		"seed traversal":      func(s *ManagedHostsState) { s.Seed.File = "../rootfs-seed.tar" },
		"absolute annotation": func(s *ManagedHostsState) { s.Seed.AnnotationValue = "/tmp/seed.tar" },
		"wrong annotation":    func(s *ManagedHostsState) { s.Seed.AnnotationKey += ".other" },
		"digest":              func(s *ManagedHostsState) { s.Seed.SHA256 = "abcd" },
		"firecracker":         func(s *ManagedHostsState) { s.Runtime = "firecracker"; s.Seed = nil },
		"external seed":       func(s *ManagedHostsState) { s.Layout = HostsLayoutExternal },
		"legacy writable":     func(s *ManagedHostsState) { s.Layout = HostsLayoutLegacy; s.Seed = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			state := testManagedHostsState()
			change(state)
			if err := WriteManagedHostsState(t.TempDir(), state); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
}

func TestManagedHostsRejectsMalformedFiles(t *testing.T) {
	valid, err := json.Marshal(testManagedHostsState())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unknown field":     strings.Replace(string(valid), "{", `{"unknown":true,`, 1),
		"missing root mode": strings.Replace(string(valid), `"root_readonly":false,`, "", 1),
		"null root mode":    strings.Replace(string(valid), `"root_readonly":false`, `"root_readonly":null`, 1),
		"trailing":          string(valid) + " {}",
		"too large":         strings.Repeat(" ", managedHostsMetadataLimit+1),
		"empty":             "",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ManagedHostsStateFile), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadManagedHostsState(dir); err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, valid, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, ManagedHostsStateFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadManagedHostsState(dir); err == nil {
			t.Fatal("symlink accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ManagedHostsStateFile), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadManagedHostsState(dir); err == nil {
			t.Fatal("directory accepted")
		}
	})
}

func TestManagedHostsRestoreCompatibility(t *testing.T) {
	state := testManagedHostsState()
	if err := ValidateManagedHostsCompatibility(state, "runsc", false, false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		runtime        string
		readonly, owns bool
	}{
		{"firecracker", false, false}, {"runsc", true, false}, {"runsc", false, true},
	} {
		if err := ValidateManagedHostsCompatibility(state, change.runtime, change.readonly, change.owns); err == nil {
			t.Fatalf("incompatible request accepted: %+v", change)
		}
	}
	state.Layout = HostsLayoutExternal
	state.Seed = nil
	if err := ValidateManagedHostsCompatibility(state, "runsc", false, false); err == nil {
		t.Fatal("missing external owner accepted")
	}
	if err := ValidateManagedHostsCompatibility(state, "runsc", false, true); err != nil {
		t.Fatal(err)
	}
}

func TestInitialFilesAndEffectiveRootReadonly(t *testing.T) {
	good := InitialFile{Path: "/etc/hosts", Mode: 0644, Content: []byte("localhost")}
	if err := ValidateInitialFiles([]InitialFile{good}); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]InitialFile{
		{good, good}, {{Path: "/etc/../etc/hosts", Mode: 0644}},
		{{Path: "/etc/hosts", Mode: 0666}}, {{Path: "/etc/hosts", Mode: 0644, Content: make([]byte, ManagedHostsContentLimit+1)}},
	} {
		if err := ValidateInitialFiles(files); err == nil {
			t.Fatal("invalid files accepted")
		}
	}
	if EffectiveRootReadonly(false, 0, nil) || EffectiveRootReadonly(true, 1, nil) || !EffectiveRootReadonly(true, 0, nil) {
		t.Fatal("effective root mode changed")
	}
	provider := &SpecUpdates{RequiresHostWritableRootfs: true}
	if EffectiveRootReadonly(true, 0, provider) || EffectiveRootReadonly(true, 1, provider) || !EffectiveRootReadonly(true, 0, &SpecUpdates{}) {
		t.Fatal("provider effective root mode changed")
	}
}
