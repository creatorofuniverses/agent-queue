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
	if err := recordResult(job, get, root, t.TempDir(), self); err != nil {
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
	if err := recordResult(job, get, root, t.TempDir(), self); err != nil {
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

func TestRecordResultDiskIO(t *testing.T) {
	root, dev, job := t.TempDir(), t.TempDir(), t.TempDir()
	cg := filepath.Join(root, "user.slice/aq-x.service")
	os.MkdirAll(cg, 0o755)
	// 259:0 is a physical disk; 252:0 is device-mapper on top of it (the same
	// bytes again) and 251:0 is zram swap: only the physical disk counts.
	os.WriteFile(filepath.Join(cg, "io.stat"), []byte(
		"251:0 rbytes=7 wbytes=9 rios=1 wios=1 dbytes=0 dios=0\n"+
			"259:0 rbytes=4096 wbytes=1048576 rios=1 wios=2 dbytes=0 dios=0\n"+
			"252:0 rbytes=4096 wbytes=1048576 rios=1 wios=2 dbytes=0 dios=0\n"), 0o644)
	os.MkdirAll(filepath.Join(dev, "259:0", "device"), 0o755)
	os.MkdirAll(filepath.Join(dev, "252:0", "dm"), 0o755)
	os.MkdirAll(filepath.Join(dev, "251:0"), 0o755)
	get := func(string) string { return "" }
	self := "0::/user.slice/aq-x.service\n"
	if err := recordResult(job, get, root, dev, self); err != nil {
		t.Fatal(err)
	}
	r, _ := ReadResultFile(job)
	if r.IOWriteBytes == nil || *r.IOWriteBytes != 1048576 || r.IOReadBytes == nil || *r.IOReadBytes != 4096 {
		t.Fatalf("%+v", r)
	}

	// No io controller (no io.stat): unknown, not zero.
	os.Remove(filepath.Join(cg, "io.stat"))
	if err := recordResult(job, get, root, dev, self); err != nil {
		t.Fatal(err)
	}
	r, _ = ReadResultFile(job)
	if r.IOWriteBytes != nil || r.IOReadBytes != nil {
		t.Fatalf("want unknown, got %+v", r)
	}

	// io controller present but nothing touched a disk: a real zero.
	os.WriteFile(filepath.Join(cg, "io.stat"), nil, 0o644)
	recordResult(job, get, root, dev, self)
	r, _ = ReadResultFile(job)
	if r.IOWriteBytes == nil || *r.IOWriteBytes != 0 {
		t.Fatalf("want 0, got %+v", r)
	}
}
