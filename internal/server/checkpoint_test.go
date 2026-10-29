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

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runtime "github.com/inclusionAI/sandboxd/api/runtime/v1"
	"github.com/inclusionAI/sandboxd/pkg/errord"
	svc "github.com/inclusionAI/sandboxd/pkg/runtime"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type checkpointTestHandler struct {
	*svc.FakeRuntimeHandler

	mu           sync.Mutex
	checkpointFn func(context.Context, svc.CheckpointConfig) error
	restoreFn    func(context.Context, svc.StartConfig) error
	checkpoints  []svc.CheckpointConfig
	restores     []svc.StartConfig
	restoreDirs  []string
	starts       []svc.StartConfig
	deleteCalls  int
}

func newCheckpointTestHandler() *checkpointTestHandler {
	return &checkpointTestHandler{FakeRuntimeHandler: svc.NewFakeRuntimeHandler()}
}

func (h *checkpointTestHandler) Start(ctx context.Context, config svc.StartConfig) error {
	h.mu.Lock()
	h.starts = append(h.starts, config)
	h.mu.Unlock()
	return nil
}

func (h *checkpointTestHandler) Delete(_ context.Context, _ string) error {
	h.mu.Lock()
	h.deleteCalls++
	h.mu.Unlock()
	return nil
}

func (h *checkpointTestHandler) Checkpoint(
	ctx context.Context,
	config svc.CheckpointConfig,
) error {
	h.mu.Lock()
	h.checkpoints = append(h.checkpoints, config)
	fn := h.checkpointFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, config)
	}
	return os.WriteFile(filepath.Join(config.Directory, "checkpoint.img"), []byte("image"), 0600)
}

func (h *checkpointTestHandler) Restore(
	ctx context.Context,
	config svc.StartConfig,
) error {
	h.mu.Lock()
	h.restores = append(h.restores, config)
	h.restoreDirs = append(h.restoreDirs, config.CheckpointDir)
	fn := h.restoreFn
	h.mu.Unlock()
	if fn != nil {
		return fn(ctx, config)
	}
	return nil
}

func storeRunningSandbox(t *testing.T, service *sandboxService, id, runtimeName string) {
	t.Helper()
	require.NoError(t, service.sandboxManager.StoreMetadata(id, &runtime.SandboxMetadata{
		ID:             id,
		RuntimeHandler: runtimeName,
	}))
}

func TestCheckpointForwardsRequest(t *testing.T) {
	handler := newCheckpointTestHandler()
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	storeRunningSandbox(t, service, "sbox-checkpoint", "runsc")
	directory := filepath.Join(t.TempDir(), "checkpoint")

	response, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:             "sbox-checkpoint",
		CheckpointDir:  directory,
		TimeoutSeconds: 5,
		Compress:       true,
		LeaveRunning:   true,
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Len(t, handler.checkpoints, 1)
	assert.Equal(t, svc.CheckpointConfig{
		ID:           "sbox-checkpoint",
		Directory:    directory,
		CgroupPath:   "",
		Compress:     true,
		LeaveRunning: true,
	}, handler.checkpoints[0])
	assert.FileExists(t, filepath.Join(directory, "checkpoint.img"))
	assert.Zero(t, handler.deleteCalls)
}

func TestCheckpointFailureCleansResidualsWithoutDeletingSource(t *testing.T) {
	handler := newCheckpointTestHandler()
	handler.checkpointFn = func(_ context.Context, config svc.CheckpointConfig) error {
		require.NoError(t, os.WriteFile(
			filepath.Join(config.Directory, "partial"),
			[]byte("partial"),
			0600,
		))
		return errors.New("runtime checkpoint failed")
	}
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	storeRunningSandbox(t, service, "sbox-failure", "runsc")
	directory := filepath.Join(t.TempDir(), "checkpoint")
	require.NoError(t, os.Mkdir(directory, 0700))

	_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:             "sbox-failure",
		CheckpointDir:  directory,
		TimeoutSeconds: 5,
		LeaveRunning:   true,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "source sandbox state is not guaranteed")
	entries, readErr := os.ReadDir(directory)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
	assert.Zero(t, handler.deleteCalls)
}

func TestCheckpointTimeoutCleansCreatedDirectory(t *testing.T) {
	handler := newCheckpointTestHandler()
	handler.checkpointFn = func(ctx context.Context, config svc.CheckpointConfig) error {
		require.NoError(t, os.WriteFile(
			filepath.Join(config.Directory, "partial"),
			[]byte("partial"),
			0600,
		))
		<-ctx.Done()
		return ctx.Err()
	}
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	storeRunningSandbox(t, service, "sbox-timeout", "runsc")
	directory := filepath.Join(t.TempDir(), "checkpoint")

	_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:             "sbox-timeout",
		CheckpointDir:  directory,
		TimeoutSeconds: 1,
	})
	require.Error(t, err)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	assert.NoDirExists(t, directory)
	assert.Zero(t, handler.deleteCalls)
}

func TestCheckpointRejectsConcurrentOperation(t *testing.T) {
	handler := newCheckpointTestHandler()
	started := make(chan struct{})
	release := make(chan struct{})
	handler.checkpointFn = func(_ context.Context, config svc.CheckpointConfig) error {
		close(started)
		<-release
		return os.WriteFile(filepath.Join(config.Directory, "checkpoint.img"), []byte("image"), 0600)
	}
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	storeRunningSandbox(t, service, "sbox-concurrent", "runsc")
	root := t.TempDir()
	firstResult := make(chan error, 1)
	go func() {
		_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
			ID:             "sbox-concurrent",
			CheckpointDir:  filepath.Join(root, "first"),
			TimeoutSeconds: 5,
		})
		firstResult <- err
	}()
	<-started

	_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:             "sbox-concurrent",
		CheckpointDir:  filepath.Join(root, "second"),
		TimeoutSeconds: 5,
	})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	close(release)
	assert.NoError(t, <-firstResult)
}

func TestCheckpointValidatesRequestAndRuntime(t *testing.T) {
	service := newTestService(t, map[string]svc.Handler{
		"runsc": svc.NewFakeRuntimeHandler(),
	})
	storeRunningSandbox(t, service, "sbox-unsupported", "runsc")

	_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:            "sbox-unsupported",
		CheckpointDir: filepath.Join(t.TempDir(), "checkpoint"),
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID:             "sbox-unsupported",
		CheckpointDir:  filepath.Join(t.TempDir(), "checkpoint"),
		TimeoutSeconds: 1,
	})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestCheckpointDirectoryValidation(t *testing.T) {
	root := t.TempDir()
	t.Run("missing leaf is created and removed on cleanup", func(t *testing.T) {
		path := filepath.Join(root, "created")
		directory, err := prepareCheckpointOutputDirectory(path)
		require.NoError(t, err)
		assert.True(t, directory.created)
		require.NoError(t, directory.cleanup())
		assert.NoDirExists(t, path)
	})

	t.Run("existing empty directory is preserved on cleanup", func(t *testing.T) {
		path := filepath.Join(root, "existing")
		require.NoError(t, os.Mkdir(path, 0700))
		directory, err := prepareCheckpointOutputDirectory(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(path, "partial"), []byte("x"), 0600))
		require.NoError(t, directory.cleanup())
		assert.DirExists(t, path)
		entries, readErr := os.ReadDir(path)
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	})

	t.Run("nonempty output is rejected", func(t *testing.T) {
		path := filepath.Join(root, "nonempty")
		require.NoError(t, os.Mkdir(path, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(path, "owned"), []byte("x"), 0600))
		_, err := prepareCheckpointOutputDirectory(path)
		assert.ErrorIs(t, err, errord.ErrFailedPrecondition)
	})

	t.Run("relative path is rejected", func(t *testing.T) {
		_, err := prepareCheckpointOutputDirectory("relative/checkpoint")
		assert.ErrorIs(t, err, errord.ErrInvalidArgument)
	})

	t.Run("symbolic link component is rejected", func(t *testing.T) {
		target := filepath.Join(root, "target")
		link := filepath.Join(root, "link")
		require.NoError(t, os.Mkdir(target, 0700))
		require.NoError(t, os.Symlink(target, link))
		_, err := prepareCheckpointOutputDirectory(filepath.Join(link, "checkpoint"))
		assert.ErrorIs(t, err, errord.ErrInvalidArgument)
	})
}

func TestCheckpointInputDirectoryValidation(t *testing.T) {
	directory := t.TempDir()
	_, err := validateCheckpointInputDirectory(directory)
	assert.ErrorIs(t, err, errord.ErrFailedPrecondition)

	require.NoError(t, os.WriteFile(filepath.Join(directory, "checkpoint.img"), []byte("image"), 0600))
	got, err := validateCheckpointInputDirectory(directory)
	require.NoError(t, err)
	assert.Equal(t, directory, got)
}

func TestCheckpointGuestMemoryResources(t *testing.T) {
	limit := int64(256 << 20)
	swap := int64(512 << 20)
	got := checkpointGuestMemoryResources(&specs.Spec{
		Linux: &specs.Linux{
			Resources: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{
					Limit: &limit,
					Swap:  &swap,
				},
			},
		},
	})
	require.NotNil(t, got)
	assert.Equal(t, limit, got.MemoryLimitInBytes)
	assert.Equal(t, swap, got.MemorySwapLimitInBytes)

	assert.Nil(t, checkpointGuestMemoryResources(nil))
	assert.Nil(t, checkpointGuestMemoryResources(&specs.Spec{
		Linux: &specs.Linux{
			Resources: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{},
			},
		},
	}))
}

func TestStartSandboxRuntimeDispatchesRestore(t *testing.T) {
	handler := newCheckpointTestHandler()
	service := buildServiceWithOptions(AddHandler("runsc", handler))
	config := svc.StartConfig{
		ID:            "sbox-restored",
		CheckpointDir: "/checkpoints/one",
	}

	require.NoError(t, service.startSandboxRuntime(context.Background(), "runsc", config))
	require.Len(t, handler.restores, 1)
	assert.Equal(t, config, handler.restores[0])
	assert.Equal(t, "/checkpoints/one", handler.restoreDirs[0])
	assert.Empty(t, handler.starts)
}

func TestStartSandboxRuntimeUsesStartWithoutCheckpoint(t *testing.T) {
	handler := newCheckpointTestHandler()
	service := buildServiceWithOptions(AddHandler("runsc", handler))
	config := svc.StartConfig{ID: "sbox-started"}

	require.NoError(t, service.startSandboxRuntime(context.Background(), "runsc", config))
	require.Len(t, handler.starts, 1)
	assert.Equal(t, config, handler.starts[0])
	assert.Empty(t, handler.restores)
}

func TestStartSandboxRuntimeRejectsUnsupportedRestore(t *testing.T) {
	service := buildServiceWithOptions(AddHandler("runc", svc.NewFakeRuntimeHandler()))
	err := service.startSandboxRuntime(context.Background(), "runc", svc.StartConfig{
		ID:            "sbox-restored",
		CheckpointDir: "/checkpoints/one",
	})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestRestoreOverlappingInputIsRejectedBeforeCleanup(t *testing.T) {
	for _, fullRPC := range []bool{false, true} {
		for _, relation := range []string{"same", "inside", "parent"} {
			t.Run(fmt.Sprintf("rpc=%v/%s", fullRPC, relation), func(t *testing.T) {
				handler := newCheckpointTestHandler()
				service := newTestService(t, map[string]svc.Handler{"runsc": handler})
				id := "sbox-protected-input"
				bundle := filepath.Join(service.config.RootDir, "containers", id)
				checkpoint := bundle
				if relation == "inside" {
					checkpoint = filepath.Join(bundle, "checkpoint")
				}
				if relation == "parent" {
					checkpoint = filepath.Dir(bundle)
				}
				require.NoError(t, os.MkdirAll(checkpoint, 0700))
				filename := filepath.Join(checkpoint, "rootfs-seed.tar")
				original := []byte("caller-owned checkpoint input")
				require.NoError(t, os.WriteFile(filename, original, 0600))
				var err error
				if fullRPC {
					_, err = service.Start(context.Background(), &runtime.StartRequest{
						SandboxID: id, Runtime: "runsc", Rootfs: &runtime.RootfsConfig{},
						CheckpointInfo: &runtime.CheckpointInfo{CheckpointDir: checkpoint},
					})
				} else {
					err = service.startSandboxRuntime(context.Background(), "runsc", svc.StartConfig{ID: id, CheckpointDir: checkpoint})
				}
				require.ErrorContains(t, err, "overlap")
				require.Empty(t, handler.restores)
				if fullRPC {
					inventory, listErr := service.List(context.Background(), &runtime.ListSandboxesRequest{})
					require.NoError(t, listErr)
					require.Empty(t, inventory.Sandboxes)
					reserved, reserveErr := service.sandboxManager.ReserveID(id)
					require.NoError(t, reserveErr)
					require.Equal(t, id, reserved)
					service.sandboxManager.ReleaseID(id)
				}
				data, readErr := os.ReadFile(filename)
				require.NoError(t, readErr)
				require.Equal(t, original, data)
			})
		}
	}
}

// A failed competing request must never clean a checkpoint another request
// successfully published into the same caller-owned directory.
func TestCheckpointConcurrentDirectoryFailurePreservesWinner(t *testing.T) {
	handler := newCheckpointTestHandler()
	firstEntered, secondEntered := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	unblockFirst := func() { firstOnce.Do(func() { close(releaseFirst) }) }
	unblockSecond := func() { secondOnce.Do(func() { close(releaseSecond) }) }
	defer unblockFirst()
	defer unblockSecond()
	handler.checkpointFn = func(ctx context.Context, config svc.CheckpointConfig) error {
		if config.ID == "sbox-winner" {
			close(firstEntered)
			select {
			case <-releaseFirst:
				return os.WriteFile(filepath.Join(config.Directory, "checkpoint.img"), []byte("winner"), 0600)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		close(secondEntered)
		select {
		case <-releaseSecond:
			return errors.New("competing checkpoint failed")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	for _, id := range []string{"sbox-winner", "sbox-contender"} {
		storeRunningSandbox(t, service, id, "runsc")
	}
	directory := t.TempDir()
	start := func(id string) <-chan error {
		result := make(chan error, 1)
		go func() {
			_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
				ID: id, CheckpointDir: directory, TimeoutSeconds: 10,
			})
			result <- err
		}()
		return result
	}
	firstResult := start("sbox-winner")
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first checkpoint did not enter runtime")
	}
	secondResult := start("sbox-contender")
	var secondErr error
	secondFinished := false
	select {
	case <-secondEntered:
		// The buggy implementation allows both requests into the runtime.
	case secondErr = <-secondResult:
		secondFinished = true
	case <-time.After(5 * time.Second):
		t.Fatal("competing checkpoint neither entered runtime nor returned")
	}
	unblockFirst()
	require.NoError(t, <-firstResult)
	unblockSecond()
	if !secondFinished {
		secondErr = <-secondResult
	}
	assert.Equal(t, codes.FailedPrecondition, status.Code(secondErr))
	data, err := os.ReadFile(filepath.Join(directory, "checkpoint.img"))
	require.NoError(t, err, "competing cleanup removed the successful checkpoint")
	assert.Equal(t, "winner", string(data))
	assert.DirExists(t, directory, "caller-owned output directory must survive")
}

func TestCheckpointDirectoryReservations(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "checkpoint")
	require.NoError(t, os.Mkdir(active, 0700))
	for name, candidate := range map[string]string{
		"same":             active,
		"normalized alias": active + "/../checkpoint/.",
		"parent":           root,
		"child":            filepath.Join(active, "child"),
	} {
		t.Run(name, func(t *testing.T) {
			service := &sandboxService{}
			require.NoError(t, service.beginCheckpoint("first", active))
			err := service.beginCheckpoint("second", candidate)
			require.ErrorIs(t, err, errord.ErrFailedPrecondition)
			assert.ErrorContains(t, err, "already in use")
			assert.Len(t, service.checkpointing, 1)
			service.finishCheckpoint("first")
			require.NoError(t, service.beginCheckpoint("second", candidate))
			service.finishCheckpoint("second")
		})
	}
	service := &sandboxService{}
	require.NoError(t, service.beginCheckpoint("first", active))
	require.NoError(t, service.beginCheckpoint("second", active+"-suffix"))
	require.ErrorIs(t, service.beginCheckpoint("first", filepath.Join(root, "other")), errord.ErrFailedPrecondition)
	assert.Len(t, service.checkpointing, 2)
	alias := filepath.Join(root, "symlink")
	require.NoError(t, os.Symlink(active, alias))
	require.ErrorIs(t, service.beginCheckpoint("third", alias), errord.ErrInvalidArgument)
}

func TestCheckpointDisjointDirectoriesRunConcurrentlyAndReleaseOnFailure(t *testing.T) {
	handler := newCheckpointTestHandler()
	entered := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	handler.checkpointFn = func(ctx context.Context, config svc.CheckpointConfig) error {
		entered <- config.ID
		select {
		case <-release:
			if config.ID == "sbox-failure" {
				return errors.New("checkpoint failed")
			}
			return os.WriteFile(filepath.Join(config.Directory, "checkpoint.img"), []byte("independent"), 0600)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service := newTestService(t, map[string]svc.Handler{"runsc": handler})
	root := t.TempDir()
	directories := map[string]string{
		"sbox-failure": filepath.Join(root, "first"),
		"sbox-success": filepath.Join(root, "second"),
	}
	results := make(map[string]<-chan error)
	for id, directory := range directories {
		storeRunningSandbox(t, service, id, "runsc")
		require.NoError(t, os.Mkdir(directory, 0700))
		result := make(chan error, 1)
		results[id] = result
		go func() {
			_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
				ID: id, CheckpointDir: directory, TimeoutSeconds: 10,
			})
			result <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("disjoint checkpoints did not enter runtime concurrently")
		}
	}
	unblock()
	require.Error(t, <-results["sbox-failure"])
	require.NoError(t, <-results["sbox-success"])
	assert.FileExists(t, filepath.Join(directories["sbox-success"], "checkpoint.img"))
	assert.DirExists(t, directories["sbox-failure"], "caller-owned output directory must survive cleanup")
	assert.Empty(t, service.checkpointing)
	// A failed generation releases both its sandbox ID and directory reservation.
	handler.mu.Lock()
	handler.checkpointFn = nil
	handler.mu.Unlock()
	_, err := service.Checkpoint(context.Background(), &runtime.CheckpointRequest{
		ID: "sbox-failure", CheckpointDir: directories["sbox-failure"], TimeoutSeconds: 5,
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(directories["sbox-failure"], "checkpoint.img"))
}

func TestCheckpointSandboxOnlyReservationPreservesOtherRuntimes(t *testing.T) {
	service := &sandboxService{}
	require.NoError(t, service.beginCheckpoint("first", ""))
	require.ErrorIs(t, service.beginCheckpoint("first", ""), errord.ErrFailedPrecondition)
	require.NoError(t, service.beginCheckpoint("second", ""))
	require.NoError(t, service.beginCheckpoint("runsc", filepath.Join(t.TempDir(), "checkpoint")))
	service.finishCheckpoint("first")
	service.finishCheckpoint("second")
	service.finishCheckpoint("runsc")
	assert.Empty(t, service.checkpointing)
}

func TestRestoreTargetGuardDoesNotChangeOtherRuntimes(t *testing.T) {
	service := managedHostsTestService(t)
	target := filepath.Join(service.config.RootDir, "containers", "protected")
	for _, runtimeName := range []string{"firecracker", "firecracker-pvm", "runc", "kata"} {
		require.NoError(t, service.validateRestoreTarget(runtimeName, "protected", target))
	}
	require.ErrorContains(t, service.validateRestoreTarget("runsc", "protected", target), "overlap")
}
