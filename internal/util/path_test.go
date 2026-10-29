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

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreDirectoriesCannotOverlap(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{source, root, filepath.Join(source, "missing", "child"), alias, filepath.Join(alias, "child")} {
		if err := ValidateRestoreDirectorySeparation(source, target); err == nil {
			t.Fatalf("overlapping target accepted: %s", target)
		}
	}
	for _, target := range []string{filepath.Join(root, "target"), source + "-suffix"} {
		if err := ValidateRestoreDirectorySeparation(source, target); err != nil {
			t.Fatalf("disjoint target rejected: %v", err)
		}
	}
}

func TestJoinWithinRoot(t *testing.T) {
	root := t.TempDir()
	got, err := JoinWithinRoot(root, "sbox-safe", "config.json")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "sbox-safe", "config.json")
	if got != want {
		t.Fatalf("JoinWithinRoot() = %q, want %q", got, want)
	}
}

func TestJoinWithinRootRejectsEscape(t *testing.T) {
	if _, err := JoinWithinRoot(t.TempDir(), "..", "outside"); err == nil {
		t.Fatal("JoinWithinRoot() accepted a path outside the root")
	}
}

func TestJoinWithinRootRejectsRoot(t *testing.T) {
	if _, err := JoinWithinRoot(t.TempDir(), "."); err == nil {
		t.Fatal("JoinWithinRoot() accepted the root itself")
	}
}
