//go:build integration

package linux

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestIntegDockerMeterLive(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skip("no docker socket")
	}
	if err := exec.Command("docker", "image", "inspect", "alpine:3").Run(); err != nil {
		t.Skip("alpine:3 not present locally")
	}
	project := fmt.Sprintf("aqtest-%d", rand.Int63())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--label", "com.docker.compose.project="+project, "alpine:3", "sleep", "60").CombinedOutput()
	if err != nil {
		t.Skipf("cannot start container: %v: %s", err, out)
	}
	id := string(out)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id[:len(id)-1]).Run() })

	none := fmt.Sprintf("aqtest-none-%d", rand.Int63())
	m := NewDockerMeter(DockerMeterConfig{Refresh: 200 * time.Millisecond})
	m.Watch([]string{project, none})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, ok := m.Usage(project); ok && b > 0 {
			break
		}
		if time.Now().After(deadline) {
			b, ok := m.Usage(project)
			t.Fatalf("no usage within 10s: %d,%v", b, ok)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if b, ok := m.Usage(none); !ok || b != 0 {
		t.Fatalf("none project = %d,%v", b, ok)
	}
}
