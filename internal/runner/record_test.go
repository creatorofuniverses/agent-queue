package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordResult(t *testing.T) {
	root, job := t.TempDir(), t.TempDir()
	cg := filepath.Join(root, "user.slice/aq-x.service")
	os.MkdirAll(cg, 0o755)
	os.WriteFile(filepath.Join(cg, "memory.peak"), []byte("33554432\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "memory.events"), []byte("low 0\nhigh 0\nmax 5\noom 1\noom_kill 2\n"), 0o644)
	os.WriteFile(filepath.Join(job, "exec.json"), []byte(`{"logs_truncated":true}`), 0o644)
	env := map[string]string{"SERVICE_RESULT": "oom-kill", "EXIT_CODE": "killed", "EXIT_STATUS": "KILL"}
	get := func(k string) string { return env[k] }
	self := "0::/user.slice/aq-x.service\n"

	if r, err := ReadResultFile(job); r != nil || err != nil {
		t.Fatalf("want nil,nil got %v %v", r, err)
	}
	if err := recordResult(job, get, root, self); err != nil {
		t.Fatal(err)
	}
	r, err := ReadResultFile(job)
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	if r.ServiceResult != "oom-kill" || r.ExitCode != "killed" || r.ExitStatus != "KILL" ||
		r.MemoryPeak != 33554432 || r.OwnOOMEvents != 1 || r.OwnOOMKills != 2 || !r.LogsTruncated || r.EndedAt.IsZero() {
		t.Fatalf("%+v", r)
	}

	env["SERVICE_RESULT"] = "success"
	if err := recordResult(job, get, root, self); err != nil {
		t.Fatal(err)
	}
	r, _ = ReadResultFile(job)
	if r.ServiceResult != "success" {
		t.Fatalf("not overwritten: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(job, "result.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp left behind")
	}
}
