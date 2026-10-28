package stats

import (
	"github.com/creatorofuniverses/agent-queue/internal/hook"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"github.com/creatorofuniverses/agent-queue/internal/profile"
	"github.com/creatorofuniverses/agent-queue/internal/store"
	"testing"
	"time"
)

func TestSummarizeItems(t *testing.T) {
	now := time.Now()
	in := Input{Since: now.Add(-time.Hour), Until: now, Items: []*model.Item{{Kind: model.KindTask, ClientType: "codex", Priority: model.PriorityHigh, State: model.StateSucceeded, CreatedAt: now.Add(-30 * time.Second), StartedAt: now.Add(-20 * time.Second), EndedAt: now}, {Kind: model.KindTask, ClientType: "codex", State: model.StateFailed, Reason: model.ReasonOOM}, {Kind: model.KindTask, ClientType: "cli", State: model.StateQueued}}}
	s := Summarize(in)
	if len(s.Items) != 2 {
		t.Fatal(s)
	}
	var g ItemGroup
	for _, v := range s.Items {
		if v.ClientType == "codex" {
			g = v
		}
	}
	if g.Count != 2 || g.Outcomes["SUCCEEDED"] != 1 || g.Outcomes["FAILED/OOM"] != 1 || g.ByPriority["high"] != 1 || g.WaitP50 != 10 || g.RunP90 != 20 {
		t.Fatal(g)
	}
}
func TestSummarizeProfiles(t *testing.T) {
	s := Summarize(Input{Runs: []profile.Run{{Profile: "p", State: model.StateSucceeded, MemoryPeak: 200, Expected: 100, Duration: 2 * time.Second, EstDuration: time.Second, MemoryFromProfile: true}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonCommandFailed, MemoryPeak: 50, Expected: 100, Duration: time.Second, EstDuration: 2 * time.Second}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonOOM, MemoryPeak: 100, Expected: 100}, {Profile: "p", State: model.StateFailed, Reason: model.ReasonTimeout}}})
	if len(s.Profiles) != 1 {
		t.Fatal(s)
	}
	v := s.Profiles[0]
	if v.Runs != 4 || v.Usable != 2 || v.OOM != 1 || v.Timeouts != 1 || v.MemoryFromProfile != 1 || v.PeakOverExpected != 1 || v.PeakToExpectedP50 != 0.5 || v.PeakToExpectedP90 != 2 || v.DurationToEstimateP90 != 2 {
		t.Fatal(v)
	}
}
func TestSummarizeQueueAndHost(t *testing.T) {
	s := Summarize(Input{Samples: []store.HostSample{{Span: 30 * time.Second, MemAvailable: 100, Agents: 10, Queued: 1, HeadBlocker: "pool_memory", PSIFullAvg10: 1}, {Span: 600 * time.Second, MemAvailable: 200, Agents: 20, Queued: 1, HeadBlocker: "pool_memory", PSIFullAvg10: 4, SwapTotal: 100, SwapFree: 50}}})
	if s.Queue.SampledSeconds != 630 || s.Queue.NonEmptySeconds != 630 || s.Queue.BlockedSeconds["pool_memory"] != 630 || s.Host.Samples != 2 || s.Host.MemAvailableMin != 100 || s.Host.MemAvailableAvg != 195 || s.Host.AgentsAvg != 19 || s.Host.AgentsMax != 20 || s.Host.SwapUsedMax != 50 || s.Host.PSIMax != 4 {
		t.Fatal(s)
	}
}
func TestSummarizeReservationsDeliveryHookIncidents(t *testing.T) {
	now := time.Now().UTC()
	in := Input{Until: now, SliceName: "aq.slice", TeardownSliceName: "aqtd.slice", Patterns: []string{"x", "y"}, Hits: []hook.Hit{{At: now, Pattern: 1, Client: "codex"}}, Items: []*model.Item{{ID: "r", Kind: model.KindReservation, ClientType: "codex", State: model.StateReleased, ExpiredAt: now, ExtendCount: 2}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTornDown}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTeardownFailed}, {Kind: model.KindReservation, ClientType: "codex", State: model.StateTeardownUnknown}}, Events: []model.Event{{ItemID: "r", Kind: model.EventReservationForceReleased, Channel: model.ChannelWebhook, Status: model.EventSent, CreatedAt: now.Add(-time.Second), SentAt: now}, {Channel: model.ChannelClientWait, Audience: model.AudienceSession, Status: model.EventSent, CreatedAt: now.Add(-2 * time.Second), SentAt: now}}, Incidents: []model.Incident{{At: now.Add(-20 * time.Second), Kind: "psi_close"}, {At: now.Add(-10 * time.Second), Kind: "psi_open"}, {At: now.Add(-5 * time.Second), Kind: "psi_close"}, {At: now, Kind: "kernel_oom", Value: 2}, {At: now, Kind: "oomd_kill", Detail: "/x/aqtd.slice/x.service"}, {At: now, Kind: "oomd_kill", Detail: "/outside"}}}
	s := Summarize(in)
	r := s.Reservations[0]
	if r.Count != 4 || r.Active != 2 || r.Expired != 1 || r.Released != 1 || r.ForceReleased != 1 || r.TornDown != 1 || r.TeardownFailed != 1 || r.TeardownUnknown != 1 || r.Extensions != 2 {
		t.Fatal(r)
	}
	if s.Hook.Hits != 1 || s.Hook.ByPattern[1].Hits != 1 || s.Hook.ByClient["codex"] != 1 || s.Incidents.KernelOOM != 2 || s.Incidents.OOMDTasks != 1 || s.Incidents.OOMDOutside != 1 || s.Incidents.PSIEpisodes != 2 || s.Incidents.PSIClosedSeconds != 15 {
		t.Fatal(s)
	}
	for _, d := range s.Delivery {
		if d.Channel == "client_wait" && (d.Unacked != 1 || d.LatencyP50 != 2) {
			t.Fatal(d)
		}
	}
}
func TestSummarizeProfileFilter(t *testing.T) {
	s := Summarize(Input{Profile: "p"})
	if s.Queue != nil || s.Host != nil || s.Hook != nil || s.Incidents != nil || len(s.Reservations) != 0 || len(s.Delivery) != 0 {
		t.Fatal(s)
	}
}
func TestPSIEpisodeOpenAtWindowStart(t *testing.T) {
	since := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	in := Input{Since: since, Until: since.Add(time.Hour), PSIClosedAtSince: true, Incidents: []model.Incident{{At: since.Add(10 * time.Minute), Kind: "psi_open"}}}
	if g := Summarize(in).Incidents; g.PSIClosedSeconds != 600 || g.PSIEpisodes != 1 {
		t.Fatal(g)
	}
	in.Incidents = nil // closed through the whole window
	if g := Summarize(in).Incidents; g.PSIClosedSeconds != 3600 {
		t.Fatal(g)
	}
}
