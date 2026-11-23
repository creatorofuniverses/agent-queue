package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/agentsess"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/levelalarm"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/platform"
	"github.com/creatorofuniverses/agent-queue/internal/stats"
	"github.com/creatorofuniverses/agent-queue/internal/tmpscan"
	"github.com/creatorofuniverses/agent-queue/internal/units"
)

type AlertView struct {
	Name      string    `json:"name"`
	Since     time.Time `json:"since"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
}

type alertSet struct {
	mu        sync.Mutex
	levels    map[model.EventKind]*levelalarm.Level
	orphans   *levelalarm.Pass
	idleAfter time.Duration
}

func newAlertSet(cfg config.Config) *alertSet {
	window, interval := cfg.Alerts.Window, cfg.Stats.SampleInterval
	return &alertSet{
		levels: map[model.EventKind]*levelalarm.Level{
			model.EventTmpFull:       levelalarm.NewLevel(cfg.TmpScan.AlertFull, window, interval),
			model.EventSwapFull:      levelalarm.NewLevel(cfg.Host.AlertSwap, window, interval),
			model.EventSessionsHeavy: levelalarm.NewLevel(float64(cfg.Sessions.AlertMemory), window, interval),
			model.EventSessionsIdle:  levelalarm.NewLevel(float64(cfg.Sessions.AlertIdleCount), window, interval),
		},
		orphans:   levelalarm.NewPass(float64(cfg.TmpScan.AlertOrphans)),
		idleAfter: cfg.Sessions.IdleAfter,
	}
}

func (a *alertSet) observeLevel(kind model.EventKind, at time.Time, value float64, ok bool) (levelalarm.Event, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.levels[kind].Observe(at, value, ok)
}

func (a *alertSet) observeOrphans(r tmpscan.Report) (levelalarm.Event, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.orphans.Observe(r.At, float64(r.OrphanBytes)+float64(r.CandidateBytes), r.Complete)
}

func (a *alertSet) Active() []AlertView {
	a.mu.Lock()
	defer a.mu.Unlock()

	active := make([]AlertView, 0, len(a.levels)+1)
	for kind, alarm := range a.levels {
		if ev, ok := alarm.Active(); ok {
			active = append(active, AlertView{string(kind), ev.Since, ev.Value, ev.Threshold})
		}
	}
	if ev, ok := a.orphans.Active(); ok {
		active = append(active, AlertView{string(model.EventTmpOrphans), ev.Since, ev.Value, ev.Threshold})
	}
	slices.SortFunc(active, func(a, b AlertView) int { return cmp.Compare(a.Name, b.Name) })
	return active
}

var alertDescriptions = map[model.EventKind]struct {
	normal model.EventKind
	label  string
	format func(float64) string
}{
	model.EventTmpFull:       {model.EventTmpFullNormal, "/tmp usage", alertPercent},
	model.EventSwapFull:      {model.EventSwapFullNormal, "swap usage", alertPercent},
	model.EventSessionsHeavy: {model.EventSessionsHeavyNormal, "agent session memory", alertSize},
	model.EventSessionsIdle:  {model.EventSessionsIdleNormal, "idle sessions", alertCount},
	model.EventTmpOrphans:    {model.EventTmpOrphansNormal, "orphan and candidate /tmp items", alertSize},
}

func alertPercent(v float64) string { return alertCount(v) + "%" }
func alertSize(v float64) string    { return units.FormatSize(uint64(v)) }
func alertCount(v float64) string   { return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") }

func emitAlert(ctx context.Context, emit func(context.Context, model.Event), kind model.EventKind, at time.Time, ev levelalarm.Event, details string) {
	desc := alertDescriptions[kind]
	value := desc.format(ev.Value)
	var text string
	if ev.High {
		text = fmt.Sprintf("%s at %s (threshold %s).", desc.label, value, desc.format(ev.Threshold))
		if details != "" {
			text += " " + details
		}
	} else {
		kind = desc.normal
		text = fmt.Sprintf("%s back to %s after %s.", desc.label, value, shortDuration(at.Sub(ev.Since)))
	}
	emit(ctx, model.Event{ID: string(model.NewID()), Kind: kind, Audience: model.AudienceOperator,
		Detail: text, CreatedAt: at.UTC()})
}

func (d *SamplerDeps) sampleAlerts(ctx context.Context, at time.Time, mem platform.MemInfo, sessions []agentsess.Session, sessionsOK bool) {
	fs, err := d.Host.FSUsage(d.TmpRoot)
	tmpUsed := max(usedPercent(fs.Size, fs.Free), usedPercent(fs.Inodes, fs.InodesFree))
	if ev, fired := d.Alerts.observeLevel(model.EventTmpFull, at, tmpUsed, err == nil); fired && d.Emit != nil {
		var report *tmpscan.Report
		if d.LastTmp != nil {
			report = d.LastTmp.Load()
		}
		emitAlert(ctx, d.Emit, model.EventTmpFull, at, ev, tmpAlertDetails(report))
	}

	swapUsed := usedPercent(mem.SwapTotal, mem.SwapFree)
	if ev, fired := d.Alerts.observeLevel(model.EventSwapFull, at, swapUsed, mem.SwapTotal != 0); fired && d.Emit != nil {
		emitAlert(ctx, d.Emit, model.EventSwapFull, at, ev, "")
	}

	var totalBytes float64
	var idle []agentsess.Session
	idleAfter := d.Alerts.idleAfter.Seconds()
	for _, s := range sessions {
		totalBytes += float64(sessionBytes(s))
		if s.IdleSeconds != nil && max(*s.IdleSeconds, 0) >= idleAfter {
			idle = append(idle, s)
		}
	}
	if ev, fired := d.Alerts.observeLevel(model.EventSessionsHeavy, at, totalBytes, sessionsOK); fired && d.Emit != nil {
		heaviest := slices.Clone(sessions)
		slices.SortStableFunc(heaviest, func(a, b agentsess.Session) int { return cmp.Compare(sessionBytes(b), sessionBytes(a)) })
		emitAlert(ctx, d.Emit, model.EventSessionsHeavy, at, ev, sessionAlertDetails(heaviest, d.Home))
	}
	if ev, fired := d.Alerts.observeLevel(model.EventSessionsIdle, at, float64(len(idle)), sessionsOK); fired && d.Emit != nil {
		slices.SortStableFunc(idle, func(a, b agentsess.Session) int { return cmp.Compare(*b.IdleSeconds, *a.IdleSeconds) })
		emitAlert(ctx, d.Emit, model.EventSessionsIdle, at, ev, sessionAlertDetails(idle, d.Home))
	}
}

func usedPercent(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(sub(total, free)) / float64(total) * 100
}

func sessionBytes(s agentsess.Session) uint64 { return s.AgentBytes + s.RunBytes }

var compactAlertSize = strings.NewReplacer(" TiB", "T", " GiB", "G", " MiB", "M", " KiB", "K", " B", "B")

func sessionAlertDetails(sessions []agentsess.Session, home string) string {
	parts := make([]string, 0, min(len(sessions), 5))
	for _, s := range sessions[:min(len(sessions), 5)] {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("%s PID %d", s.Client, s.PID)
		}
		project := s.Cwd
		if project == "" {
			project = "unknown"
		}
		projectLabel := stats.AgentLabel("agent:project:"+project, home)
		idle := "unknown"
		if s.IdleSeconds != nil {
			idle = shortDuration(time.Duration(max(*s.IdleSeconds, 0) * float64(time.Second)))
		}
		size := compactAlertSize.Replace(units.FormatSize(sessionBytes(s)))
		parts = append(parts, fmt.Sprintf("%s (%s, %s, idle %s)", name, projectLabel, size, idle))
	}
	return strings.Join(parts, ", ")
}

func tmpAlertDetails(report *tmpscan.Report) string {
	var items []string
	if report != nil {
		for _, item := range report.Items { // Reports are ordered by size.
			if item.State == "orphan" || item.State == "candidate" {
				items = append(items, fmt.Sprintf("%s (%s, %s)", item.Path, units.FormatSize(item.Bytes), item.State))
				if len(items) == 5 {
					break
				}
			}
		}
	}
	if len(items) == 0 {
		return "See aq tmp."
	}
	return "Largest orphan and candidate items include " + strings.Join(items, ", ") + ". See aq tmp."
}
