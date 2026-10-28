// Package stats builds pure summaries of dispatcher history.
package stats

import (
	"resource-dispatcher/internal/hook"
	"resource-dispatcher/internal/model"
	"resource-dispatcher/internal/profile"
	"resource-dispatcher/internal/store"
	"sort"
	"strings"
	"time"
)

type Input struct {
	Since, Until                 time.Time
	Profile                      string
	Items                        []*model.Item
	Runs                         []profile.Run
	Events                       []model.Event
	Samples                      []store.HostSample
	Incidents                    []model.Incident
	Hits                         []hook.Hit
	Patterns                     []string
	SliceName, TeardownSliceName string
	// PSIClosedAtSince: admission was closed by pressure when the window began.
	PSIClosedAtSince bool
}
type Summary struct {
	Since        time.Time          `json:"since"`
	Until        time.Time          `json:"until"`
	Profile      string             `json:"profile"`
	Items        []ItemGroup        `json:"items"`
	Profiles     []ProfileStats     `json:"profiles"`
	Queue        *QueueStats        `json:"queue"`
	Host         *HostStats         `json:"host"`
	Reservations []ReservationGroup `json:"reservations"`
	Delivery     []ChannelStats     `json:"delivery"`
	Hook         *HookStats         `json:"hook"`
	Incidents    *IncidentStats     `json:"incidents"`
}
type ItemGroup struct {
	Kind       string         `json:"kind"`
	ClientType string         `json:"client_type"`
	Count      int            `json:"count"`
	Outcomes   map[string]int `json:"outcomes"`
	ByPriority map[string]int `json:"by_priority"`
	WaitP50    float64        `json:"wait_p50"`
	WaitP90    float64        `json:"wait_p90"`
	RunP50     float64        `json:"run_p50"`
	RunP90     float64        `json:"run_p90"`
}
type ProfileStats struct {
	Profile               string  `json:"profile"`
	Runs                  int     `json:"runs"`
	Usable                int     `json:"usable"`
	OOM                   int     `json:"oom"`
	Timeouts              int     `json:"timeouts"`
	MemoryFromProfile     int     `json:"memory_from_profile"`
	PeakOverExpected      int     `json:"peak_over_expected"`
	PeakToExpectedP50     float64 `json:"peak_to_expected_p50"`
	PeakToExpectedP90     float64 `json:"peak_to_expected_p90"`
	DurationToEstimateP50 float64 `json:"duration_to_estimate_p50"`
	DurationToEstimateP90 float64 `json:"duration_to_estimate_p90"`
}
type QueueStats struct {
	SampledSeconds  float64            `json:"sampled_seconds"`
	NonEmptySeconds float64            `json:"non_empty_seconds"`
	BlockedSeconds  map[string]float64 `json:"blocked_seconds"`
}
type HostStats struct {
	Samples         int     `json:"samples"`
	MemAvailableMin uint64  `json:"mem_available_min"`
	MemAvailableAvg uint64  `json:"mem_available_avg"`
	SwapUsedMax     uint64  `json:"swap_used_max"`
	PSIMax          float64 `json:"psi_max"`
	AgentsAvg       uint64  `json:"agents_avg"`
	AgentsMax       uint64  `json:"agents_max"`
	DockerAvg       uint64  `json:"docker_avg"`
	DockerMax       uint64  `json:"docker_max"`
	TasksAvg        uint64  `json:"tasks_avg"`
	TasksMax        uint64  `json:"tasks_max"`
	OtherAvg        uint64  `json:"other_avg"`
	OtherMax        uint64  `json:"other_max"`
	CommittedAvg    uint64  `json:"committed_avg"`
	CommittedMax    uint64  `json:"committed_max"`
	UnconsumedAvg   uint64  `json:"unconsumed_avg"`
	UnconsumedMax   uint64  `json:"unconsumed_max"`
}
type ReservationGroup struct {
	ClientType      string `json:"client_type"`
	Count           int    `json:"count"`
	Active          int    `json:"active"`
	Expired         int    `json:"expired"`
	Released        int    `json:"released"`
	ForceReleased   int    `json:"force_released"`
	TornDown        int    `json:"torn_down"`
	TeardownFailed  int    `json:"teardown_failed"`
	TeardownUnknown int    `json:"teardown_unknown"`
	Extensions      int    `json:"extensions"`
}
type ChannelStats struct {
	Channel    string  `json:"channel"`
	Events     int     `json:"events"`
	Sent       int     `json:"sent"`
	Acked      int     `json:"acked"`
	Pending    int     `json:"pending"`
	Failed     int     `json:"failed"`
	Skipped    int     `json:"skipped"`
	Unacked    int     `json:"unacked"`
	LatencyP50 float64 `json:"latency_p50"`
	LatencyP90 float64 `json:"latency_p90"`
}
type HookStats struct {
	Hits      int            `json:"hits"`
	ByPattern []PatternHits  `json:"by_pattern"`
	ByClient  map[string]int `json:"by_client"`
}
type PatternHits struct {
	Index   int    `json:"index"`
	Pattern string `json:"pattern"`
	Hits    int    `json:"hits"`
}
type IncidentStats struct {
	KernelOOM        float64 `json:"kernel_oom"`
	OOMDOutside      int     `json:"oomd_outside"`
	OOMDTasks        int     `json:"oomd_tasks"`
	PSIEpisodes      int     `json:"psi_episodes"`
	PSIClosedSeconds float64 `json:"psi_closed_seconds"`
}

func Summarize(in Input) Summary {
	s := Summary{Since: in.Since, Until: in.Until, Profile: in.Profile, Items: []ItemGroup{}, Profiles: []ProfileStats{}}
	summarizeItems(&s, in)
	summarizeProfiles(&s, in)
	if in.Profile != "" {
		return s
	}
	summarizeHost(&s, in)
	summarizeReservations(&s, in)
	summarizeDelivery(&s, in)
	s.Hook = &HookStats{ByClient: map[string]int{}}
	for i, p := range in.Patterns {
		s.Hook.ByPattern = append(s.Hook.ByPattern, PatternHits{Index: i, Pattern: p})
	}
	for _, h := range in.Hits {
		s.Hook.Hits++
		s.Hook.ByClient[h.Client]++
		if h.Pattern >= 0 && h.Pattern < len(s.Hook.ByPattern) {
			s.Hook.ByPattern[h.Pattern].Hits++
		}
	}
	summarizeIncidents(&s, in)
	return s
}
func keys[T any](m map[string]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
func summarizeItems(s *Summary, in Input) {
	type acc struct {
		g         ItemGroup
		wait, run []float64
	}
	groups := map[string]*acc{}
	for _, it := range in.Items {
		if in.Profile != "" && it.Profile != in.Profile {
			continue
		}
		k := string(it.Kind) + "\x00" + it.ClientType
		a := groups[k]
		if a == nil {
			a = &acc{g: ItemGroup{Kind: string(it.Kind), ClientType: it.ClientType, Outcomes: map[string]int{}, ByPriority: map[string]int{}}}
			groups[k] = a
		}
		a.g.Count++
		outcome := "active"
		if it.State.Terminal() {
			outcome = string(it.State)
			if it.Reason != "" {
				outcome += "/" + string(it.Reason)
			}
		}
		a.g.Outcomes[outcome]++
		a.g.ByPriority[it.Priority.String()]++
		if !it.StartedAt.IsZero() {
			a.wait = append(a.wait, max(0, it.StartedAt.Sub(it.CreatedAt).Seconds()))
			if !it.EndedAt.IsZero() {
				a.run = append(a.run, max(0, it.EndedAt.Sub(it.StartedAt).Seconds()))
			}
		}
	}
	for _, k := range keys(groups) {
		a := groups[k]
		a.g.WaitP50 = profile.Percentile(a.wait, 50)
		a.g.WaitP90 = profile.Percentile(a.wait, 90)
		a.g.RunP50 = profile.Percentile(a.run, 50)
		a.g.RunP90 = profile.Percentile(a.run, 90)
		s.Items = append(s.Items, a.g)
	}
}
func summarizeProfiles(s *Summary, in Input) {
	type acc struct {
		g              ProfileStats
		peak, duration []float64
	}
	groups := map[string]*acc{}
	for _, r := range in.Runs {
		if in.Profile != "" && r.Profile != in.Profile {
			continue
		}
		a := groups[r.Profile]
		if a == nil {
			a = &acc{g: ProfileStats{Profile: r.Profile}}
			groups[r.Profile] = a
		}
		a.g.Runs++
		if strings.HasPrefix(string(r.Reason), "OOM") {
			a.g.OOM++
		}
		if r.Reason == model.ReasonTimeout {
			a.g.Timeouts++
		}
		if r.MemoryFromProfile {
			a.g.MemoryFromProfile++
		}
		if r.Expected > 0 && r.MemoryPeak > r.Expected {
			a.g.PeakOverExpected++
		}
		if r.Usable() {
			a.g.Usable++
			if r.Expected > 0 {
				a.peak = append(a.peak, float64(r.MemoryPeak)/float64(r.Expected))
			}
			if r.EstDuration > 0 {
				a.duration = append(a.duration, float64(r.Duration)/float64(r.EstDuration))
			}
		}
	}
	for _, k := range keys(groups) {
		a := groups[k]
		a.g.PeakToExpectedP50 = profile.Percentile(a.peak, 50)
		a.g.PeakToExpectedP90 = profile.Percentile(a.peak, 90)
		a.g.DurationToEstimateP50 = profile.Percentile(a.duration, 50)
		a.g.DurationToEstimateP90 = profile.Percentile(a.duration, 90)
		s.Profiles = append(s.Profiles, a.g)
	}
}
func summarizeHost(s *Summary, in Input) {
	q := &QueueStats{BlockedSeconds: map[string]float64{}}
	h := &HostStats{Samples: len(in.Samples)}
	s.Queue = q
	s.Host = h
	var avg [8]float64
	for i, v := range in.Samples {
		span := v.Span.Seconds()
		q.SampledSeconds += span
		if v.Queued > 0 {
			q.NonEmptySeconds += span
		}
		if v.HeadBlocker != "" {
			q.BlockedSeconds[v.HeadBlocker] += span
		}
		if i == 0 || v.MemAvailable < h.MemAvailableMin {
			h.MemAvailableMin = v.MemAvailable
		}
		h.SwapUsedMax = max(h.SwapUsedMax, satSub(v.SwapTotal, v.SwapFree))
		h.PSIMax = max(h.PSIMax, v.PSIFullAvg10)
		unc := v.TasksUnconsumed + v.StandsUnconsumed + v.BudgetsUnconsumed
		vals := []uint64{v.MemAvailable, v.Agents, v.Docker, v.Tasks, v.Other, v.CommittedMemory, unc}
		for i, n := range vals {
			avg[i] += float64(n) * span
		}
		h.AgentsMax = max(h.AgentsMax, v.Agents)
		h.DockerMax = max(h.DockerMax, v.Docker)
		h.TasksMax = max(h.TasksMax, v.Tasks)
		h.OtherMax = max(h.OtherMax, v.Other)
		h.CommittedMax = max(h.CommittedMax, v.CommittedMemory)
		h.UnconsumedMax = max(h.UnconsumedMax, unc)
	}
	if q.SampledSeconds > 0 {
		dst := []*uint64{&h.MemAvailableAvg, &h.AgentsAvg, &h.DockerAvg, &h.TasksAvg, &h.OtherAvg, &h.CommittedAvg, &h.UnconsumedAvg}
		for i, p := range dst {
			*p = uint64(avg[i] / q.SampledSeconds)
		}
	}
}
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
func summarizeReservations(s *Summary, in Input) {
	groups := map[string]*ReservationGroup{}
	clients := map[model.ItemID]string{}
	for _, it := range in.Items {
		if !it.IsReservation() {
			continue
		}
		g := groups[it.ClientType]
		if g == nil {
			g = &ReservationGroup{ClientType: it.ClientType}
			groups[it.ClientType] = g
		}
		clients[it.ID] = it.ClientType
		g.Count++
		if !it.State.Terminal() {
			g.Active++
		}
		if !it.ExpiredAt.IsZero() {
			g.Expired++
		}
		g.Extensions += it.ExtendCount
		switch it.State {
		case model.StateReleased:
			g.Released++
		case model.StateTornDown:
			g.TornDown++
		case model.StateTeardownFailed:
			g.TeardownFailed++
		case model.StateTeardownUnknown:
			g.TeardownUnknown++
		}
	}
	for _, e := range in.Events {
		if e.Kind == model.EventReservationForceReleased {
			if client, ok := clients[e.ItemID]; ok {
				groups[client].ForceReleased++
			}
		}
	}
	for _, k := range keys(groups) {
		s.Reservations = append(s.Reservations, *groups[k])
	}
}
func summarizeDelivery(s *Summary, in Input) {
	type acc struct {
		g       ChannelStats
		latency []float64
	}
	groups := map[string]*acc{}
	for _, e := range in.Events {
		k := string(e.Channel)
		a := groups[k]
		if a == nil {
			a = &acc{g: ChannelStats{Channel: k}}
			groups[k] = a
		}
		a.g.Events++
		switch e.Status {
		case model.EventSent:
			a.g.Sent++
			if e.Audience == model.AudienceSession {
				a.g.Unacked++
			}
		case model.EventAcked:
			a.g.Acked++
		case model.EventPending, model.EventDelivering:
			a.g.Pending++
		case model.EventFailed:
			a.g.Failed++
		case model.EventSkipped:
			a.g.Skipped++
		}
		if !e.SentAt.IsZero() {
			a.latency = append(a.latency, max(0, e.SentAt.Sub(e.CreatedAt).Seconds()))
		}
	}
	for _, k := range keys(groups) {
		a := groups[k]
		a.g.LatencyP50 = profile.Percentile(a.latency, 50)
		a.g.LatencyP90 = profile.Percentile(a.latency, 90)
		s.Delivery = append(s.Delivery, a.g)
	}
}
func summarizeIncidents(s *Summary, in Input) {
	g := &IncidentStats{}
	s.Incidents = g
	incs := append([]model.Incident(nil), in.Incidents...)
	sort.SliceStable(incs, func(i, j int) bool { return incs[i].At.Before(incs[j].At) })
	var closed time.Time
	if in.PSIClosedAtSince {
		closed = in.Since
		g.PSIEpisodes++
	}
	for _, v := range incs {
		switch v.Kind {
		case "kernel_oom":
			g.KernelOOM += v.Value
		case "oomd_kill":
			own := false
			for _, slice := range []string{in.SliceName, in.TeardownSliceName} {
				if slice != "" && strings.Contains(v.Detail, "/"+slice+"/") {
					own = true
				}
			}
			if own {
				g.OOMDTasks++
			} else {
				g.OOMDOutside++
			}
		case "psi_close":
			if closed.IsZero() {
				closed = v.At
				g.PSIEpisodes++
			}
		case "psi_open":
			if !closed.IsZero() {
				g.PSIClosedSeconds += max(0, v.At.Sub(closed).Seconds())
				closed = time.Time{}
			}
		}
	}
	if !closed.IsZero() {
		g.PSIClosedSeconds += max(0, in.Until.Sub(closed).Seconds())
	}
}
