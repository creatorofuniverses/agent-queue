package runner

import "io"

// capWriter writes at most max bytes to w and silently discards the rest,
// so the producer never blocks on a full pipe.
type capWriter struct {
	w         io.Writer
	remaining uint64
	truncated bool
}

func newCapWriter(w io.Writer, max uint64) *capWriter {
	return &capWriter{w: w, remaining: max}
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if uint64(len(p)) > c.remaining {
		c.truncated = true
		p = p[:c.remaining]
	}
	if len(p) > 0 {
		if _, err := c.w.Write(p); err != nil {
			return 0, err
		}
		c.remaining -= uint64(len(p))
	}
	return n, nil
}
