package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"agent-queue/internal/model"
)

// userManagerCgroup returns the user@UID.service cgroup (relative to the cgroup root)
// from /proc/self/cgroup content; outside the user manager it falls back to the standard layout.
func userManagerCgroup(selfCgroup string, uid int) string {
	for _, line := range strings.Split(selfCgroup, "\n") {
		p, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		parts := strings.Split(strings.Trim(strings.TrimSpace(p), "/"), "/")
		for i, part := range parts {
			if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
				return strings.Join(parts[:i+1], "/")
			}
		}
	}
	return fmt.Sprintf("user.slice/user-%d.slice/user@%d.service", uid, uid)
}

// sliceCgroupPath maps a slice name to its nested cgroup path: "a-b.slice" lives in "a.slice/a-b.slice".
func sliceCgroupPath(slice string) string {
	name := strings.TrimSuffix(slice, ".slice")
	parts := strings.Split(name, "-")
	dirs := make([]string, len(parts))
	for i := range parts {
		dirs[i] = strings.Join(parts[:i+1], "-") + ".slice"
	}
	return filepath.Join(dirs...)
}

// sliceDir is the task slice's cgroup: the pool and the guard's ceiling.
func (e *Executor) sliceDir() string { return e.sliceDirOf(e.cfg.SliceName) }

func (e *Executor) teardownSliceDir() string { return e.sliceDirOf(e.cfg.TeardownSlice) }

func (e *Executor) sliceDirOf(slice string) string {
	return filepath.Join(e.cfg.CgroupRoot, e.manager, sliceCgroupPath(slice))
}

// unitSlice is the slice a unit runs in: teardown units have their own.
func (c ExecutorConfig) unitSlice(id model.ItemID) string {
	if model.IsTeardownUnitID(id) {
		return c.TeardownSlice
	}
	return c.SliceName
}

func (e *Executor) unitName(id model.ItemID) string {
	return e.cfg.UnitPrefix + string(id) + ".service"
}

func (e *Executor) unitDir(id model.ItemID) string {
	return filepath.Join(e.sliceDirOf(e.cfg.unitSlice(id)), e.unitName(id))
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func readEvent(path, key string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok && k == key {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no %s", path, key)
}

// Alive reads "populated" from cgroup.events, which also counts processes in
// sub-cgroups the task created (the unit's own cgroup.procs would miss them).
func (e *Executor) Alive(_ context.Context, id model.ItemID) (bool, error) {
	n, err := readEvent(filepath.Join(e.unitDir(id), "cgroup.events"), "populated")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (e *Executor) MemoryCurrent(id model.ItemID) (uint64, error) {
	v, err := readUint(filepath.Join(e.unitDir(id), "memory.current"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return v, err
}

// ListManaged lists the prefixed units of the task slice and of the teardown slice
// (sorted; Stop finds a unit by name whatever its slice).
func (e *Executor) ListManaged(_ context.Context) ([]model.ItemID, error) {
	var ids []model.ItemID
	for _, slice := range []string{e.cfg.SliceName, e.cfg.TeardownSlice} {
		if slice == "" {
			continue
		}
		ents, err := os.ReadDir(e.sliceDirOf(slice))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ent := range ents {
			name := ent.Name()
			if !ent.IsDir() || !strings.HasPrefix(name, e.cfg.UnitPrefix) || !strings.HasSuffix(name, ".service") {
				continue
			}
			ids = append(ids, model.ItemID(strings.TrimSuffix(strings.TrimPrefix(name, e.cfg.UnitPrefix), ".service")))
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil // a stray unit of one name in both slices is listed once
}

func (e *Executor) SliceCurrent() (uint64, error) {
	return readUint(filepath.Join(e.sliceDir(), "memory.current"))
}

func (e *Executor) SliceOOMKills() (uint64, error) {
	return readEvent(filepath.Join(e.sliceDir(), "memory.events"), "oom_kill")
}

// SliceCeiling reads the slice's actual memory.max ("max" means unlimited).
func (e *Executor) SliceCeiling() (uint64, error) {
	b, err := os.ReadFile(filepath.Join(e.sliceDir(), "memory.max"))
	if err != nil {
		return 0, err
	}
	if v := strings.TrimSpace(string(b)); v != "max" {
		return strconv.ParseUint(v, 10, 64)
	}
	return math.MaxUint64, nil
}

// SetSliceCeiling writes the slice cgroup directly (no systemd round trip); EnsureSlice resets it.
func (e *Executor) SetSliceCeiling(max, high uint64) error {
	dir := e.sliceDir()
	if err := writeUint(filepath.Join(dir, "memory.high"), high); err != nil {
		return err
	}
	return writeUint(filepath.Join(dir, "memory.max"), max)
}

// writeUint writes an existing cgroup file without creating or truncating it.
// It uses a plain blocking fd: os.File registers cgroup files with the poller and
// makes them O_NONBLOCK, and the kernel then skips the reclaim/OOM step of a
// memory.max write, so lowering the limit would not kill anything.
func writeUint(path string, v uint64) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	buf := strconv.AppendUint(make([]byte, 0, 20), v, 10)
	_, err = unix.Write(fd, buf)
	if cerr := unix.Close(fd); err == nil {
		err = cerr
	}
	if err != nil {
		return &os.PathError{Op: "write", Path: path, Err: err}
	}
	return nil
}

func (e *Executor) TeardownSliceOOMKills() (uint64, error) {
	return readEvent(filepath.Join(e.teardownSliceDir(), "memory.events"), "oom_kill")
}
