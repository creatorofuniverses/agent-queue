package linux

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type dockerFixture struct {
	sock, root string
	srv        *httptest.Server
	mu         sync.Mutex
	filters    []string
	body       map[string]string // project -> JSON body
}

func newDockerFixture(t *testing.T) *dockerFixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "aqd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &dockerFixture{sock: filepath.Join(dir, "d.sock"), root: filepath.Join(dir, "cg"), body: map[string]string{}}
	l, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flt := r.URL.Query().Get("filters")
		f.mu.Lock()
		f.filters = append(f.filters, flt)
		body := "[]"
		for p, b := range f.body {
			if strings.Contains(flt, "com.docker.compose.project="+p+`"`) {
				body = b
			}
		}
		f.mu.Unlock()
		w.Write([]byte(body))
	}))
	f.srv.Listener = l
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *dockerFixture) cgroup(t *testing.T, rel string, current, inactive string) {
	t.Helper()
	d := filepath.Join(f.root, rel)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "memory.current"), []byte(current+"\n"), 0o644)
	os.WriteFile(filepath.Join(d, "memory.stat"), []byte("anon 1\ninactive_file "+inactive+"\nactive_file 7\n"), 0o644)
}

func (f *dockerFixture) meter(now *time.Time) *DockerMeter {
	return NewDockerMeter(DockerMeterConfig{Socket: f.sock, CgroupRoot: f.root, Now: func() time.Time { return *now }})
}

func TestDockerMeterSumsProject(t *testing.T) {
	f := newDockerFixture(t)
	f.body["p"] = `[{"Id":"aaa"},{"Id":"bbb"},{"Id":"ccc"}]`
	f.cgroup(t, "system.slice/docker-aaa.scope", "104857600", "4194304")
	f.cgroup(t, "docker/bbb", "10485760", "0")
	now := time.Unix(1000, 0)
	m := f.meter(&now)
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	got, ok := m.Usage("p")
	if want := uint64(100<<20 - 4<<20 + 10<<20); !ok || got != want {
		t.Fatalf("Usage = %d,%v want %d", got, ok, want)
	}
}

func TestDockerMeterEmptyProject(t *testing.T) {
	f := newDockerFixture(t)
	f.body["p"] = `[]`
	now := time.Unix(1000, 0)
	m := f.meter(&now)
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	if got, ok := m.Usage("p"); !ok || got != 0 {
		t.Fatalf("Usage = %d,%v", got, ok)
	}
}

func TestDockerMeterUnreachableGoesStale(t *testing.T) {
	f := newDockerFixture(t)
	f.body["p"] = `[{"Id":"aaa"}]`
	f.cgroup(t, "system.slice/docker-aaa.scope", "2048", "0")
	now := time.Unix(1000, 0)
	m := f.meter(&now)
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	f.srv.Close()
	now = now.Add(29 * time.Second)
	m.refresh(context.Background())
	if got, ok := m.Usage("p"); !ok || got != 2048 {
		t.Fatalf("29s: %d,%v", got, ok)
	}
	now = now.Add(2 * time.Second)
	if _, ok := m.Usage("p"); ok {
		t.Fatal("31s: want stale")
	}
}

func TestDockerMeterNeverMeasured(t *testing.T) {
	d, _ := os.MkdirTemp("", "aqd")
	t.Cleanup(func() { os.RemoveAll(d) })
	now := time.Unix(1000, 0)
	m := NewDockerMeter(DockerMeterConfig{Socket: filepath.Join(d, "none.sock"), CgroupRoot: d, Now: func() time.Time { return now }})
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	if _, ok := m.Usage("p"); ok {
		t.Fatal("want not ok")
	}
}

func TestDockerMeterWatchReplaces(t *testing.T) {
	f := newDockerFixture(t)
	now := time.Unix(1000, 0)
	m := f.meter(&now)
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	m.Watch([]string{"q"})
	m.refresh(context.Background())
	if _, ok := m.Usage("p"); ok {
		t.Fatal("p should be forgotten")
	}
	if _, ok := m.Usage("q"); !ok {
		t.Fatal("q should be measured")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.filters) != 2 || !strings.Contains(f.filters[1], `=q"`) || strings.Contains(f.filters[1], `=p"`) {
		t.Fatalf("filters = %v", f.filters)
	}
}

func TestInactiveFileSaturates(t *testing.T) {
	f := newDockerFixture(t)
	f.cgroup(t, "system.slice/docker-aaa.scope", "100", "500")
	got, err := containerMemory(f.root, "aaa")
	if err != nil || got != 0 {
		t.Fatalf("got %d,%v", got, err)
	}
}
