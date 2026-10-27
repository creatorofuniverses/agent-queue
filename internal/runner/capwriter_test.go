package runner

import (
	"bytes"
	"errors"
	"testing"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCapWriterBoundary(t *testing.T) {
	var b bytes.Buffer
	c := newCapWriter(&b, 4)
	if n, err := c.Write([]byte("abcd")); n != 4 || err != nil || c.truncated {
		t.Fatalf("exact cap: %d %v %v", n, err, c.truncated)
	}
	if n, err := c.Write([]byte("ef")); n != 2 || err != nil || !c.truncated {
		t.Fatalf("over cap: %d %v %v", n, err, c.truncated)
	}
	if b.String() != "abcd" {
		t.Fatalf("got %q", b.String())
	}
}

func TestCapWriterWriteError(t *testing.T) {
	c := newCapWriter(failWriter{}, 100)
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil || !c.truncated {
		t.Fatalf("%d %v %v", n, err, c.truncated)
	}
	if n, err := c.Write([]byte("def")); n != 3 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}
