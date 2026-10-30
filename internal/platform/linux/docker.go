package linux

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creatorofuniverses/agent-queue/internal/platform"
)

// DockerMeterConfig configures the compose-project meter.
type DockerMeterConfig struct {
	Socket         string        // default /var/run/docker.sock
	CgroupRoot     string        // default /sys/fs/cgroup
	Refresh        time.Duration // default 5s
	Stale          time.Duration // default 30s
	RequestTimeout time.Duration // default 2s
	Now            func() time.Time
}

type standSample struct {
	bytes      uint64
	containers []platform.ContainerUsage
	at         time.Time
}

// DockerMeter measures compose projects through the Docker Engine API and cgroupfs.
// Usage only reads the cache; all I/O happens in Run.
type DockerMeter struct {
	cfg    DockerMeterConfig
	client *http.Client

	mu      sync.Mutex
	watched []string
	samples map[string]standSample
	// added wakes Run when Watch adds a project: until it is measured its stand
	// counts as wholly unused, so it should not wait for the next tick.
	added chan struct{}
}

func NewDockerMeter(cfg DockerMeterConfig) *DockerMeter {
	if cfg.Socket == "" {
		cfg.Socket = "/var/run/docker.sock"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 5 * time.Second
	}
	if cfg.Stale <= 0 {
		cfg.Stale = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	sock := cfg.Socket
	return &DockerMeter{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
				DisableKeepAlives: true,
			},
		},
		samples: map[string]standSample{},
		added:   make(chan struct{}, 1),
	}
}

// Watch replaces the set of projects to measure; others are forgotten.
func (m *DockerMeter) Watch(projects []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := slices.ContainsFunc(projects, func(p string) bool { return !slices.Contains(m.watched, p) })
	m.watched = append([]string(nil), projects...)
	keep := make(map[string]standSample, len(projects))
	for _, p := range projects {
		if s, ok := m.samples[p]; ok {
			keep[p] = s
		}
	}
	m.samples = keep
	if added {
		select {
		case m.added <- struct{}{}:
		default: // a refresh is already due
		}
	}
}

// Usage returns the last measurement if it is not older than Stale.
func (m *DockerMeter) Usage(project string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.samples[project]
	if !ok || m.cfg.Now().Sub(s.at) > m.cfg.Stale {
		return 0, false
	}
	return s.bytes, true
}

func (m *DockerMeter) Containers(project string) ([]platform.ContainerUsage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.samples[project]
	if !ok || m.cfg.Now().Sub(s.at) > m.cfg.Stale {
		return nil, false
	}
	return slices.Clone(s.containers), true
}

// Run refreshes immediately and then every Refresh until ctx ends.
func (m *DockerMeter) Run(ctx context.Context) {
	m.refresh(ctx)
	t := time.NewTicker(m.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refresh(ctx)
		case <-m.added:
			m.refresh(ctx)
		}
	}
}

func (m *DockerMeter) refresh(ctx context.Context) {
	m.mu.Lock()
	projects := append([]string(nil), m.watched...)
	m.mu.Unlock()
	for _, p := range projects {
		cs, err := m.measure(ctx, p)
		if err != nil {
			continue
		}
		var b uint64
		for _, c := range cs {
			b += c.Bytes
		}
		m.mu.Lock()
		// Only record if the project is still watched.
		for _, w := range m.watched {
			if w == p {
				m.samples[p] = standSample{bytes: b, containers: cs, at: m.cfg.Now()}
				break
			}
		}
		m.mu.Unlock()
	}
}

// measure is each running container of project with its memory, largest first.
func (m *DockerMeter) measure(ctx context.Context, project string) ([]platform.ContainerUsage, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	u := "http://docker/containers/json?all=0&filters=" + url.QueryEscape(string(filters))
	ctx, cancel := context.WithTimeout(ctx, m.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker api: %s", resp.Status)
	}
	var list []struct {
		Id     string
		Names  []string
		Labels map[string]string
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := make([]platform.ContainerUsage, 0, len(list))
	for _, c := range list {
		b, err := containerMemory(m.cfg.CgroupRoot, c.Id)
		if err != nil {
			return nil, err
		}
		name := c.Labels["com.docker.compose.service"]
		if name == "" && len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == "" {
			name = c.Id[:min(12, len(c.Id))]
		}
		out = append(out, platform.ContainerUsage{Name: name, Bytes: b})
	}
	slices.SortStableFunc(out, func(a, b platform.ContainerUsage) int { return cmp.Compare(b.Bytes, a.Bytes) })
	return out, nil
}

// containerMemory is memory.current minus inactive_file (saturating) of the container's cgroup,
// under either the systemd or the cgroupfs driver layout; no cgroup means 0.
func containerMemory(cgroupRoot, id string) (uint64, error) {
	if id == "" || strings.ContainsRune(id, '/') {
		return 0, fmt.Errorf("bad container id %q", id)
	}
	for _, dir := range []string{
		filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope"),
		filepath.Join(cgroupRoot, "docker", id),
	} {
		cur, err := readUint(filepath.Join(dir, "memory.current"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		inactive, err := statValue(filepath.Join(dir, "memory.stat"), "inactive_file")
		if errors.Is(err, fs.ErrNotExist) {
			continue // container vanished between the two reads
		}
		if err != nil {
			return 0, err
		}
		if inactive >= cur {
			return 0, nil
		}
		return cur - inactive, nil
	}
	return 0, nil
}

func statValue(path, key string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, sc.Err()
}

var _ platform.StandMeter = (*DockerMeter)(nil)
