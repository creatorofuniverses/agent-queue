package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"resource-dispatcher/internal/model"
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

func (e *Executor) sliceDir() string {
	return filepath.Join(e.cfg.CgroupRoot, e.manager, sliceCgroupPath(e.cfg.SliceName))
}

func (e *Executor) unitName(id model.ItemID) string {
	return e.cfg.UnitPrefix + string(id) + ".service"
}

func (e *Executor) unitDir(id model.ItemID) string {
	return filepath.Join(e.sliceDir(), e.unitName(id))
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

func (e *Executor) Alive(_ context.Context, id model.ItemID) (bool, error) {
	b, err := os.ReadFile(filepath.Join(e.unitDir(id), "cgroup.procs"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(b))) > 0, nil
}

func (e *Executor) MemoryCurrent(id model.ItemID) (uint64, error) {
	v, err := readUint(filepath.Join(e.unitDir(id), "memory.current"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return v, err
}

func (e *Executor) ListManaged(_ context.Context) ([]model.ItemID, error) {
	ents, err := os.ReadDir(e.sliceDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []model.ItemID
	for _, ent := range ents {
		name := ent.Name()
		if !ent.IsDir() || !strings.HasPrefix(name, e.cfg.UnitPrefix) || !strings.HasSuffix(name, ".service") {
			continue
		}
		ids = append(ids, model.ItemID(strings.TrimSuffix(strings.TrimPrefix(name, e.cfg.UnitPrefix), ".service")))
	}
	return ids, nil
}

func (e *Executor) SliceCurrent() (uint64, error) {
	return readUint(filepath.Join(e.sliceDir(), "memory.current"))
}

func (e *Executor) SliceOOMKills() (uint64, error) {
	return readEvent(filepath.Join(e.sliceDir(), "memory.events"), "oom_kill")
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
func writeUint(path string, v uint64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.FormatUint(v, 10))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
