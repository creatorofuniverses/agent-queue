package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"resource-dispatcher/internal/model"
	"testing"
	"time"
)

func event(id string, ch model.Channel, at time.Time) model.Event {
	return model.Event{ID: id, ItemID: "i", Kind: model.EventTaskFinished, Audience: model.AudienceSession, SessionID: "S", Channel: ch, Status: model.EventPending, CreatedAt: at}
}
func TestMigration0003SkipsOldEvents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "aq.db")
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"0001_init.sql", "0002_reservations.sql"} {
		b, e := os.ReadFile("migrations/" + n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(b)); e != nil {
			t.Fatal(e)
		}
	}
	_, e = db.Exec(`INSERT INTO events(id,item_id,kind,audience,created_at) VALUES('old','i','task_finished','session',''); PRAGMA user_version=2`)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	evs, e := s.Events(context.Background(), "i")
	if e != nil || len(evs) != 1 || evs[0].Status != model.EventSkipped || evs[0].Channel != model.ChannelNone {
		t.Fatal(evs, e)
	}
}
func TestEventRoundTripDeliveryFields(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	it := newItem(0, at)
	it.NoNotify = true
	it.FinishNotified = true
	s.InsertItem(ctx, it)
	e := event("e", model.ChannelWebhook, at)
	e.ItemID = it.ID
	e.Attempts = 3
	e.NextAttemptAt = at
	e.SentAt = at
	e.AckedAt = at
	e.LastError = "x"
	if err := s.UpdateWithEvents(ctx, it, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(ctx, it.ID)
	if err != nil || !reflect.DeepEqual(got, []model.Event{e}) {
		t.Fatal(got, err)
	}
	item, _ := s.Get(ctx, it.ID)
	if !item.NoNotify || !item.FinishNotified {
		t.Fatal(item)
	}
	e.ID = "e2"
	e.NextAttemptAt = time.Time{}
	if err := s.AddEvents(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Events(ctx, it.ID)
	if !got[1].NextAttemptAt.Equal(at) {
		t.Fatal(got)
	}
}
func TestClaimDueOrderAndChannel(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	a := event("a", model.ChannelWebhook, at)
	b := event("b", model.ExecChannel("codex"), at)
	c := event("c", model.ChannelWebhook, at)
	c.NextAttemptAt = at.Add(time.Hour)
	if e := s.AddEvents(ctx, []model.Event{a, b, c}); e != nil {
		t.Fatal(e)
	}
	got, e := s.ClaimDue(ctx, model.ChannelWebhook, at)
	if e != nil || got == nil || got.ID != "a" || got.Status != model.EventDelivering {
		t.Fatal(got, e)
	}
	got, e = s.ClaimDue(ctx, model.ChannelWebhook, at)
	if e != nil || got != nil {
		t.Fatal(got, e)
	}
	s.MarkSent(ctx, "a", at)
	next, ok, e := s.NextDue(ctx, model.ChannelWebhook)
	if e != nil || !ok || !next.Equal(c.NextAttemptAt) {
		t.Fatal(next, ok, e)
	}
}
func TestResetDelivering(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	s.AddEvents(ctx, []model.Event{event("e", model.ChannelWebhook, at)})
	s.ClaimDue(ctx, model.ChannelWebhook, at)
	n, e := s.ResetDelivering(ctx)
	if n != 1 || e != nil {
		t.Fatal(n, e)
	}
	got, e := s.ClaimDue(ctx, model.ChannelWebhook, at)
	if got == nil || e != nil {
		t.Fatal(got, e)
	}
}
func TestMarkRacesWithAck(t *testing.T) {
	for _, op := range []string{"sent", "retry", "failed", "before"} {
		t.Run(op, func(t *testing.T) {
			s := openTemp(t)
			ctx := context.Background()
			at := time.Now().UTC()
			s.AddEvents(ctx, []model.Event{event("e", model.ChannelWebhook, at)})
			if op != "before" {
				s.ClaimDue(ctx, model.ChannelWebhook, at)
			}
			s.Ack(ctx, "e", at)
			switch op {
			case "sent":
				s.MarkSent(ctx, "e", at)
			case "retry":
				s.MarkRetry(ctx, "e", 1, at, "x")
			case "failed":
				s.MarkFailed(ctx, "e", 1, "x", []model.Event{event("extra", model.ChannelWebhook, at)})
			case "before":
				got, e := s.ClaimDue(ctx, model.ChannelWebhook, at)
				if got != nil || e != nil {
					t.Fatal(got, e)
				}
			}
			got, _ := s.Events(ctx, "i")
			if len(got) != 1 || got[0].Status != model.EventAcked {
				t.Fatal(got)
			}
		})
	}
}
func TestMarkFailedWritesDeliveryFailed(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	s.AddEvents(ctx, []model.Event{event("e", model.ChannelWebhook, at)})
	s.ClaimDue(ctx, model.ChannelWebhook, at)
	e2 := event("df", model.ChannelWebhook, at)
	e2.Kind = model.EventDeliveryFailed
	if e := s.MarkFailed(ctx, "e", 7, "boom", []model.Event{e2}); e != nil {
		t.Fatal(e)
	}
	got, _ := s.Events(ctx, "i")
	if len(got) != 2 || got[0].Attempts != 7 || got[0].Status != model.EventFailed {
		t.Fatal(got)
	}
	if last, e := s.LastDeliveryError(ctx, model.ChannelWebhook); e != nil || last != "boom" {
		t.Fatal(last, e)
	}
}
func TestMarkClientWaitSent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	a := event("a", model.ChannelClientWait, at)
	b := event("b", model.ChannelClientWait, at)
	b.Kind = model.EventReservationGranted
	c := event("c", model.ExecChannel("codex"), at)
	s.AddEvents(ctx, []model.Event{a, b, c})
	got, e := s.MarkClientWaitSent(ctx, "i", []model.EventKind{model.EventTaskFinished}, at)
	if e != nil || len(got) != 1 || got[0].ID != "a" || got[0].Status != model.EventSent {
		t.Fatal(got, e)
	}
	got, e = s.MarkClientWaitSent(ctx, "i", []model.EventKind{model.EventTaskFinished}, at)
	if e != nil || len(got) != 0 {
		t.Fatal(got, e)
	}
	if ok, e := s.HasPendingClientWait(ctx, "i", model.EventReservationGranted); !ok || e != nil {
		t.Fatal(ok, e)
	}
}
func TestAckIdempotent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	s.AddEvents(ctx, []model.Event{event("e", model.ChannelWebhook, at)})
	s.Ack(ctx, "e", at)
	got, e := s.Ack(ctx, "e", at.Add(time.Hour))
	if e != nil || !got.AckedAt.Equal(at) {
		t.Fatal(got, e)
	}
	_, e = s.Ack(ctx, "absent", at)
	if !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
func TestListWithUnacked(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Now().UTC()
	for i, st := range []model.State{model.StateSucceeded, model.StateFailed, model.StateRunning} {
		it := newItem(0, at)
		it.SessionID = "S"
		it.State = st
		s.InsertItem(ctx, it)
		e := event(string(rune('a'+i)), model.ChannelClientWait, at)
		e.ItemID = it.ID
		if i == 1 {
			e.Status = model.EventAcked
		}
		s.AddEvents(ctx, []model.Event{e})
	}
	got, e := s.List(ctx, ListFilter{SessionID: "S", WithUnacked: true})
	if e != nil || len(got) != 2 {
		t.Fatal(got, e)
	}
	evs, e := s.UnackedSessionEvents(ctx, "S")
	if e != nil || len(evs) != 2 {
		t.Fatal(evs, e)
	}
}
