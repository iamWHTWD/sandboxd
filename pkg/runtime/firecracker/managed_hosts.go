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
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/inclusionAI/sandboxd/config"
	runtimecore "github.com/inclusionAI/sandboxd/pkg/runtime"
)

func cloneFirecrackerManagedHosts(state *runtimecore.ManagedHostsState) *runtimecore.ManagedHostsState {
	if state == nil {
		return nil
	}
	copy := *state
	return &copy
}

func validateFirecrackerManagedHosts(state *runtimecore.ManagedHostsState) error {
	if err := runtimecore.ValidateManagedHostsState(state); err != nil {
		return err
	}
	if state.Runtime != config.RuntimeNameFirecracker {
		return errors.New("Firecracker managed hosts metadata has a different runtime")
	}
	return nil
}

func (handler *Handler) validateManagedHostsRuntime(state *runtimecore.ManagedHostsState) error {
	if state == nil {
		return nil
	}
	if handler.runtimeName != config.RuntimeNameFirecracker {
		return errors.New("managed hosts are supported only by the standard Firecracker runtime")
	}
	return validateFirecrackerManagedHosts(state)
}

func firecrackerMountOwnsHosts(target string) bool {
	target = filepath.Clean(target)
	return target == "/etc/hosts" || strings.HasPrefix("/etc/hosts", strings.TrimSuffix(target, "/")+"/")
}

// Validate the final OCI layout before opening resources. Initial files are
// cold-boot input only; restored guests already own their files in the overlay.
func validateFirecrackerHostsLayout(spec *runtimecore.Spec, start runtimecore.StartConfig, mounts []runtimecore.Mount, extra firecrackerExtraConfig) error {
	if err := runtimecore.ValidateInitialFiles(start.InitialFiles); err != nil {
		return err
	}
	state := start.ManagedHosts
	if state == nil {
		if len(start.InitialFiles) != 0 {
			return errors.New("Firecracker initial files require managed hosts metadata")
		}
		return nil
	}
	if err := validateFirecrackerManagedHosts(state); err != nil {
		return err
	}
	if spec.Root.Readonly != state.RootReadonly {
		return errors.New("Firecracker managed hosts root mode does not match the final OCI root")
	}
	owned, readonly := false, false
	for _, mount := range mounts {
		if state.Layout == runtimecore.HostsLayoutPrivateRootfs && firecrackerMountTargetsOverlap(mount.Destination, "/etc/hosts") {
			return fmt.Errorf("Firecracker managed hosts conflict with mount %s", mount.Destination)
		}
		if firecrackerMountOwnsHosts(mount.Destination) {
			owned = true
			readonly = slices.Contains(mount.Options, "ro") && !slices.Contains(mount.Options, "rw")
		}
	}
	for _, mount := range extra.NativeWritableMounts {
		if state.Layout != runtimecore.HostsLayoutExternal && firecrackerMountTargetsOverlap(mount.Target, "/etc/hosts") {
			return errors.New("Firecracker native writable mounts cannot overlap managed hosts")
		}
		if firecrackerMountOwnsHosts(mount.Target) {
			owned, readonly = true, false
		}
	}
	switch state.Layout {
	case runtimecore.HostsLayoutPrivateRootfs:
		if start.CheckpointDir == "" && len(start.InitialFiles) != 1 {
			return errors.New("Firecracker managed hosts initialization is missing")
		}
	case runtimecore.HostsLayoutLegacy:
		if !owned || !readonly {
			return errors.New("Firecracker legacy hosts require a readonly hosts mount")
		}
	case runtimecore.HostsLayoutExternal:
		if !owned {
			return errors.New("Firecracker external hosts require a covering mount")
		}
	}
	if len(start.InitialFiles) > 0 && (start.CheckpointDir != "" || state.Layout != runtimecore.HostsLayoutPrivateRootfs) {
		return errors.New("Firecracker initial hosts are only supported for fresh private-rootfs sandboxes")
	}
	return nil
}

func (handler *Handler) restoreManagedHosts(start *runtimecore.StartConfig, artifact *firecrackerCheckpointArtifact) error {
	if len(start.InitialFiles) != 0 {
		return errors.New("Firecracker restore must not reinitialize hosts")
	}
	saved := artifact.ManagedHosts
	if err := handler.validateManagedHostsRuntime(saved); err != nil {
		return err
	}
	if start.ManagedHosts != nil {
		if err := handler.validateManagedHostsRuntime(start.ManagedHosts); err != nil {
			return err
		}
		if saved == nil || *saved != *start.ManagedHosts {
			return errors.New("Firecracker restore hosts layout does not match checkpoint metadata")
		}
	}
	start.ManagedHosts = cloneFirecrackerManagedHosts(saved)
	return nil
}

// The fixed declaration, sidecar and mandatory digest must agree. Unlike large
// components this tiny file is always hashed, including on warm restores.
func readFirecrackerCheckpointHosts(dir string, manifest *firecrackerCheckpointManifest) (*runtimecore.ManagedHostsState, error) {
	state, err := runtimecore.ReadManagedHostsState(dir)
	if err != nil {
		return nil, err
	}
	declared := manifest != nil && manifest.ManagedHosts != ""
	if declared && manifest.ManagedHosts != runtimecore.ManagedHostsStateFile {
		return nil, errors.New("Firecracker checkpoint has an invalid managed hosts sidecar declaration")
	}
	if declared != (state != nil) {
		return nil, errors.New("Firecracker checkpoint managed hosts declaration and sidecar disagree")
	}
	if !declared {
		if manifest != nil {
			if _, exists := manifest.Digests[runtimecore.ManagedHostsStateFile]; exists {
				return nil, errors.New("Firecracker checkpoint has an undeclared managed hosts digest")
			}
		}
		return nil, nil
	}
	if err := validateFirecrackerManagedHosts(state); err != nil {
		return nil, err
	}
	expected := manifest.Digests[runtimecore.ManagedHostsStateFile]
	if !isFirecrackerCompatDigest(expected) {
		return nil, errors.New("Firecracker checkpoint managed hosts SHA-256 is missing or invalid")
	}
	digest, err := digestFirecrackerCheckpointComponent(context.Background(), runtimecore.ManagedHostsStateFile, filepath.Join(dir, runtimecore.ManagedHostsStateFile))
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New("Firecracker checkpoint managed hosts digest mismatch")
	}
	return state, nil
}
