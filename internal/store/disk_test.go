package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestDiskWritesUpsert(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	h := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	rows := []DiskWrite{
		{Hour: h.Add(5 * time.Minute), Source: "host", Bytes: 100},
		{Hour: h.Add(40 * time.Minute), Source: "host", Bytes: 50},
		{Hour: h, Source: "agent:codex:p", Within: "build", Bytes: 70},
	}
	if e := s.AddDiskWrites(ctx, rows); e != nil {
		t.Fatal(e)
	}
	if e := s.AddDiskWrites(ctx, []DiskWrite{{Hour: h, Source: "host", Bytes: -30}}); e != nil {
		t.Fatal(e)
	}
	got, e := s.DiskWritesSince(ctx, h)
	if e != nil {
		t.Fatal(e)
	}
	want := []DiskWrite{
		{Hour: h, Source: "agent:codex:p", Within: "build", Bytes: 70},
		{Hour: h, Source: "host", Bytes: 120},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if e := s.AddDiskWrites(ctx, nil); e != nil {
		t.Fatal(e)
	}
}

func TestDiskWritesSinceHourBoundary(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	h := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s.AddDiskWrites(ctx, []DiskWrite{{Hour: h.Add(-time.Hour), Source: "host", Bytes: 1}, {Hour: h, Source: "host", Bytes: 2}, {Hour: h.Add(time.Hour), Source: "host", Bytes: 3}})
	got, e := s.DiskWritesSince(ctx, h.Add(45*time.Minute))
	if e != nil || len(got) != 2 || got[0].Bytes != 2 || got[1].Bytes != 3 || !got[0].Hour.Equal(h) {
		t.Fatalf("%+v %v", got, e)
	}
}

func TestDiskWritesRetention(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	h := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s.AddDiskWrites(ctx, []DiskWrite{{Hour: h.Add(-time.Hour), Source: "host", Bytes: 1}, {Hour: h, Source: "host", Bytes: 2}})
	if e := s.DeleteDiskWrites(ctx, h); e != nil {
		t.Fatal(e)
	}
	got, _ := s.DiskWritesSince(ctx, time.Time{})
	if len(got) != 1 || got[0].Bytes != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestExportDisk(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	h := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s.AddDiskWrites(ctx, []DiskWrite{{Hour: h, Source: "agent:codex:p", Within: "build", Bytes: 7}})
	var cols []string
	var all [][]any
	if e := s.Export(ctx, "disk", h, func(c []string, r [][]any) error { cols = c; all = append(all, r...); return nil }); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(cols, []string{"hour", "source", "within", "bytes"}) || len(all) != 1 || all[0][1] != "agent:codex:p" || all[0][3] != int64(7) {
		t.Fatal(cols, all)
	}
	found := false
	for _, n := range ExportTables {
		found = found || n == "disk"
	}
	if !found {
		t.Fatal("disk not in ExportTables")
	}
}
