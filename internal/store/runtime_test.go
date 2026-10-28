package store

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeState(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, ok, err := s.LoadRuntime(ctx, "k"); ok || err != nil {
		t.Fatal(ok, err)
	}
	now := time.Now()
	s.SaveRuntime(ctx, "k", []byte(`{"a":1}`), now)
	s.SaveRuntime(ctx, "k", []byte(`{"a":2}`), now)
	if v, ok, err := s.LoadRuntime(ctx, "k"); !ok || err != nil || string(v) != `{"a":2}` {
		t.Fatal(string(v), ok, err)
	}
	if err := s.DeleteRuntime(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadRuntime(ctx, "k"); ok {
		t.Fatal("not deleted")
	}
}
