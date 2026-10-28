package daemon

import (
	"context"
	"errors"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/hook"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"github.com/creatorofuniverses/agent-queue/internal/tmpdir"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func RunRetention(ctx context.Context, st *store.Store, paths config.Paths, cfg config.Config, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if e := retentionStep(ctx, st, paths, cfg, time.Now().UTC()); e != nil && ctx.Err() == nil {
			slog.Warn("retention: step", "err", e)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
func dirSize(dir string) (uint64, error) {
	var n uint64
	e := filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if !d.IsDir() {
			info, e := d.Info()
			if os.IsNotExist(e) {
				return nil // removed during the walk
			}
			if e != nil {
				return e
			}
			n += uint64(info.Size())
		}
		return nil
	})
	return n, e
}
func retentionStep(ctx context.Context, st *store.Store, paths config.Paths, cfg config.Config, now time.Time) error {
	active, e := st.ActiveIDs(ctx)
	if e != nil {
		return e
	}
	// Temp dirs live under TmpRoot (tmpfs) and, for tasks started before it was
	// set, under the state dir.
	sweep := []struct{ subdir, dir string }{
		{"jobs", filepath.Join(paths.StateDir, "jobs")},
		{"tmp", filepath.Join(paths.StateDir, "tmp")},
	}
	if paths.TmpRoot != "" {
		sweep = append(sweep, struct{ subdir, dir string }{"tmp", paths.TmpRoot})
	}
	for _, sw := range sweep {
		subdir, dir := sw.subdir, sw.dir
		entries, e := os.ReadDir(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if e := ctx.Err(); e != nil {
				return e
			}
			info, e := entry.Info()
			if e != nil {
				continue
			}
			if !info.ModTime().Before(now.Add(-time.Hour)) {
				continue
			}
			id := entry.Name()
			if subdir == "tmp" {
				if rest, ok := strings.CutPrefix(id, "teardown-"); ok {
					if i := strings.LastIndex(rest, "-"); i >= 0 {
						id = rest[:i]
					}
				}
				if active[model.ItemID(id)] {
					continue
				}
			} else {
				has, e := st.HasItem(ctx, model.ItemID(id))
				if e != nil {
					return e
				}
				if has {
					continue
				}
			}
			// Temp dirs may hold read-only dirs a task left; job dirs are ours.
			if e = tmpdir.RemoveAll(filepath.Join(dir, entry.Name())); e != nil {
				slog.Warn("retention: remove orphan", "path", filepath.Join(dir, entry.Name()), "err", e)
			}
		}
	}
	candidates, e := st.PruneCandidates(ctx)
	if e != nil {
		return e
	}
	total, e := dirSize(filepath.Join(paths.StateDir, "jobs"))
	if e != nil {
		// Unknown size: prune by age only, and keep the history and sample stages.
		slog.Warn("retention: measure jobs", "err", e)
		total = 0
	}
	for _, v := range candidates {
		if e := ctx.Err(); e != nil {
			return e
		}
		if !v.EndedAt.Before(now.Add(-cfg.Retention.LogsMaxAge)) && total <= cfg.Retention.LogsMaxTotal {
			continue
		}
		dir := paths.JobDir(v.ID)
		size, e := dirSize(dir)
		if e != nil {
			slog.Warn("retention: size logs", "id", v.ID, "err", e)
			continue
		}
		if e = os.RemoveAll(dir); e != nil {
			slog.Warn("retention: remove logs", "id", v.ID, "err", e)
			continue
		}
		total = sub(total, size)
		if e = st.MarkLogsPruned(ctx, []model.ItemID{v.ID}); e != nil {
			return e
		}
	}
	before := now.Add(-cfg.Retention.HistoryMaxAge)
	files, e := hook.RotatedHitFiles(paths.StateDir)
	if e != nil {
		return e
	}
	for _, p := range files {
		info, e := os.Stat(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			slog.Warn("retention: stat hook", "err", e)
			continue
		}
		if info.ModTime().Before(before) {
			if e = os.Remove(p); e != nil && !errors.Is(e, fs.ErrNotExist) {
				slog.Warn("retention: remove hook", "err", e)
			}
		}
	}
	// A deleted row turns its jobs/ dir into an orphan, so history never goes
	// before the logs are due (owner decision: clamp rather than reject config).
	if _, e = st.DeleteHistory(ctx, now.Add(-max(cfg.Retention.HistoryMaxAge, cfg.Retention.LogsMaxAge))); e != nil {
		return e
	}
	if e = st.ThinHostSamples(ctx, now.Add(-cfg.Retention.SamplesFullRes)); e != nil {
		return e
	}
	return st.DeleteHostSamples(ctx, now.Add(-cfg.Retention.SamplesMaxAge))
}
