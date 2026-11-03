package linux

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
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

func TestDockerMeterBreakdown(t *testing.T) {
	f := newDockerFixture(t)
	f.body["p"] = `[{"Id":"aaa","Names":["/p-db-1"],"Labels":{"com.docker.compose.service":"db"}},{"Id":"bbb","Names":["/runner"]},{"Id":"ccc"}]`
	f.cgroup(t, "system.slice/docker-aaa.scope", "10485760", "0")
	f.cgroup(t, "docker/bbb", "104857600", "4194304")
	now := time.Unix(1000, 0)
	m := f.meter(&now)
	m.Watch([]string{"p"})
	m.refresh(context.Background())
	got, ok := m.Containers("p")
	want := []platform.ContainerUsage{{Name: "runner", Bytes: 96 << 20}, {Name: "db", Bytes: 10 << 20}, {Name: "ccc", Bytes: 0}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Containers = %+v,%v want %+v", got, ok, want)
	}
	now = now.Add(time.Hour)
	if _, ok := m.Containers("p"); ok {
		t.Fatal("a stale breakdown")
	}
}

// After a restart the first pass watches the held stands; until they are
// measured each counts as wholly unused, so waiting for the next tick skews
// admission and forecasts for seconds.
func TestDockerMeterMeasuresNewProjectAtOnce(t *testing.T) {
	f := newDockerFixture(t)
	f.body["p"] = `[]`
	m := NewDockerMeter(DockerMeterConfig{Socket: f.sock, CgroupRoot: f.root, Refresh: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	time.Sleep(50 * time.Millisecond) // Run's first refresh finds nothing to measure
	m.Watch([]string{"p"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := m.Usage("p"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a newly watched project waited for the next refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDockerMeterSources(t *testing.T) {
	f := newDockerFixture(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" || r.URL.Query().Get("all") != "0" || r.URL.Query().Get("filters") != "" {
			http.Error(w, "unexpected "+r.URL.String(), 400)
			return
		}
		w.Write([]byte(`[{"Id":"aaa","Names":["/a"],"Labels":{"com.docker.compose.project":"runi"}},{"Id":"bbb","Names":["/lonely"]},{"Id":"ccc123456789012345"}]`))
	})
	now := time.Unix(1000, 0)
	got, err := f.meter(&now).Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"aaa": "docker:runi", "bbb": "docker:lonely", "ccc123456789012345": "docker:ccc123456789"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
