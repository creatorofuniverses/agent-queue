package scheduler

import (
	"context"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/admission"
	"github.com/creatorofuniverses/agent-queue/internal/model"
	"log/slog"
	"time"
)

func (s *Scheduler) alertCleaningLocked(ctx context.Context, it *model.Item, now time.Time) {
	since, ok := s.cleaningSince[it.ID]
	if !ok {
		since = now
		s.cleaningSince[it.ID] = now
	}
	if s.cleaningAlerted[it.ID] || now.Sub(since) < s.d.Cfg.Notify.CleaningStuckAlert {
		return
	}
	evs := s.newEvents(it, model.EventCleaningStuck, fmt.Sprintf("result read %s ago, processes still alive", now.Sub(since)), model.AudienceOperator)
	if err := s.persistLocked(ctx, it, evs...); err != nil {
		slog.Error("scheduler: cleaning alert", "err", err)
		return
	}
	s.cleaningAlerted[it.ID] = true
}
func (s *Scheduler) alertAdmissionLocked(ctx context.Context, head model.ItemID, dec admission.Decision, now time.Time) {
	switch dec.Blocker {
	case admission.BlockRecovering, admission.BlockPressure, admission.BlockDisk, admission.BlockTmp, admission.BlockLiveMemory:
	default:
		s.closedSince = time.Time{}
		s.closedAlerted = false
		return
	}
	if head == "" {
		s.closedSince = time.Time{}
		s.closedAlerted = false
		return
	}
	if s.closedSince.IsZero() {
		s.closedSince = now
	}
	if s.closedAlerted || now.Sub(s.closedSince) < s.d.Cfg.Notify.AdmissionClosedAlert {
		return
	}
	// Use item metadata when available, without another external operation.
	it, err := s.d.Store.Get(ctx, head)
	if err != nil {
		slog.Error("scheduler: admission alert item", "err", err)
		return
	}
	evs := s.newEvents(it, model.EventAdmissionClosed, fmt.Sprintf("%s: %s for %s", dec.Blocker, dec.Detail, now.Sub(s.closedSince)), model.AudienceOperator)
	for i := range evs {
		evs[i].CreatedAt = now
	}
	if err := s.d.Store.AddEvents(ctx, evs); err != nil {
		slog.Error("scheduler: admission alert", "err", err)
		return
	}
	s.closedAlerted = true
	if len(evs) > 0 && s.d.OnEvents != nil {
		s.d.OnEvents()
	}
}
