package daemon

import (
	"agent-queue/internal/model"
	"agent-queue/internal/protocol"
	"context"
	"encoding/json"
	"time"
)

type ProfileView struct {
	Profile  string               `json:"profile"`
	Estimate *ProfileEstimateView `json:"estimate,omitempty"`
	Runs     []ProfileRunView     `json:"runs"`
}
type ProfileEstimateView struct {
	Runs            int     `json:"runs"`
	Limit           uint64  `json:"limit"`
	Expected        uint64  `json:"expected"`
	MaxPeak         uint64  `json:"max_peak"`
	DurationSeconds float64 `json:"duration_seconds"`
}
type ProfileRunView struct {
	ItemID            model.ItemID `json:"item_id"`
	EndedAt           time.Time    `json:"ended_at"`
	State             model.State  `json:"state"`
	Reason            model.Reason `json:"reason"`
	Usable            bool         `json:"usable"`
	DurationSeconds   float64      `json:"duration_seconds"`
	MemoryPeak        uint64       `json:"memory_peak"`
	SampleP90         uint64       `json:"sample_p90"`
	Samples           int          `json:"samples"`
	Limit             uint64       `json:"limit"`
	Expected          uint64       `json:"expected"`
	MemoryFromProfile bool         `json:"memory_from_profile"`
}
type ProfileListEntry struct {
	Profile   string    `json:"profile"`
	Runs      int       `json:"runs"`
	Usable    int       `json:"usable"`
	LastEnded time.Time `json:"last_ended"`
}

func (d *server) profileShow(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Profile string `json:"profile"`
	}
	if e := decode(raw, &a); e != nil {
		return nil, e
	}
	rs, e := d.st.ProfileRuns(ctx, a.Profile, 20)
	if e != nil {
		return nil, e
	}
	if len(rs) == 0 {
		return nil, &protocol.Error{Code: protocol.CodeNotFound, Message: "profile not found"}
	}
	v := ProfileView{Profile: a.Profile, Runs: []ProfileRunView{}}
	est, ok, e := d.sched.ProfileEstimate(ctx, a.Profile)
	if e != nil {
		return nil, e
	}
	if ok {
		v.Estimate = &ProfileEstimateView{est.Runs, est.Limit, est.Expected, est.MaxPeak, est.Duration.Seconds()}
	}
	for _, r := range rs {
		v.Runs = append(v.Runs, ProfileRunView{r.ItemID, r.EndedAt, r.State, r.Reason, r.Usable(), r.Duration.Seconds(), r.MemoryPeak, r.Hist.Quantile(90), r.Samples, r.Limit, r.Expected, r.MemoryFromProfile})
	}
	return v, nil
}
func (d *server) profileList(ctx context.Context) (any, error) {
	ps, e := d.st.Profiles(ctx)
	if e != nil {
		return nil, e
	}
	out := []ProfileListEntry{}
	for _, p := range ps {
		out = append(out, ProfileListEntry{p.Name, p.Runs, p.Usable, p.LastEnded})
	}
	return out, nil
}
