package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creatorofuniverses/agent-queue/internal/config"
)

// The hints come from the config as aq loads it. What misled the text matching
// of the installer is ordinary TOML here.
func TestInstallHints(t *testing.T) {
	const (
		path         = "/cfg/config.toml"
		noOpenClaude = "hint: agent_processes in " + path + " does not list openclaude.\n" +
			"      add it, or OpenClaude's sessions are not counted and its commands belong to cli or to the session of an agent it was started from.\n"
		noOpenCode = "hint: agent_processes in " + path + " does not list opencode.\n" +
			"      add it, or OpenCode's processes are not counted and its commands are identified by the environment alone: an inherited variable of another agent can file them under that agent's session.\n"
		noAdapter = "hint: [[agents]] in " + path + " has no adapter for OpenCode.\n" +
			"      add one with identity_env = \"OPENCODE_SESSION_ID\" and delivery = \"client-wait\", or OpenCode's commands belong to cli and its MCP submissions are rejected with \"session identity unknown\".\n"
		twoAdapters = "hint: [[agents]] in " + path + " has 2 adapters with identity_env = \"OPENCODE_SESSION_ID\".\n" +
			"      keep exactly one, or OpenCode's MCP submissions are rejected with \"session identity unknown\".\n"
		all        = "[stats]\nagent_processes = [\"claude\", \"openclaude\", \"codex\", \"opencode\"]\n"
		allButName = "[stats]\nagent_processes = [\"claude\", \"codex\"]\n"
	)
	tests := []struct {
		name, config string
		want, not    []string
	}{
		{"default config", "[stats]\nsample_interval = \"30s\"\n", nil, []string{"hint:"}},
		{"a list of its own without both names", allButName, []string{noOpenClaude, noOpenCode}, []string{"[[agents]]"}},
		{"names only in a comment", "[stats]\nagent_processes = [\"claude\", \"codex\"] # \"openclaude\", \"opencode\"\n", []string{noOpenClaude, noOpenCode}, nil},
		{"a name counts as a whole string", "[stats]\nagent_processes = [\"claude\", \"openclaude-preview\", \"opencode-preview\"]\n", []string{noOpenClaude, noOpenCode}, nil},
		{"a quoted key", "[stats]\n\"agent_processes\" = [\"claude\", \"openclaude\", \"codex\"]\n", []string{noOpenCode}, []string{"does not list openclaude"}},
		{"an empty list of names", "[stats]\nagent_processes = []\n", []string{noOpenClaude, noOpenCode}, nil},
		{"own adapters without the OpenCode one", all + "\n[[agents]]\nname = \"claude\"\nidentity_env = \"CLAUDE_CODE_SESSION_ID\"\n", []string{noAdapter}, []string{"agent_processes"}},
		{"agents = [] keeps the default adapters", "agents = []\n", nil, []string{"hint:"}},
		{"agents = [] and a list of its own", "agents = []\n" + allButName, []string{noOpenClaude, noOpenCode}, []string{"[[agents]]"}},
		{"a header with a quoted key", "[[\"agents\"]]\nname = \"claude\"\nidentity_env = \"CLAUDE_CODE_SESSION_ID\"\n", []string{noAdapter}, nil},
		{"a command that contains the assignment", "[[agents]]\nname = \"codex\"\nidentity_env = \"CODEX_THREAD_ID\"\ndelivery = \"push\"\ncommand = [\"codex\", \"identity_env = 'OPENCODE_SESSION_ID'\", \"{session}\", \"{text}\"]\n", []string{noAdapter}, nil},
		{"a longer variable name", all + "\n[[agents]]\nname = \"opencode\"\nidentity_env = \"OPENCODE_SESSION_ID_OLD\"\ndelivery = \"client-wait\"\n", []string{noAdapter}, nil},
		{"a commented-out block", "# [[agents]]\n# name = \"claude\"\n# identity_env = \"CLAUDE_CODE_SESSION_ID\"\n", nil, []string{"hint:"}},
		{"the adapter as an inline table", "agents = [ { name = 'opencode', identity_env = 'OPENCODE_SESSION_ID', delivery = 'client-wait' } ]\n", nil, []string{"hint:"}},
		{"two adapters on the variable", "[[agents]]\nname = \"opencode\"\nidentity_env = \"OPENCODE_SESSION_ID\"\ndelivery = \"client-wait\"\n\n[[agents]]\nname = \"opencode2\"\nidentity_env = \"OPENCODE_SESSION_ID\"\ndelivery = \"client-wait\"\n", []string{twoAdapters}, []string{"has no adapter"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(file, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(file)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(installHints(cfg, path), "")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output lacks %q;\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("output has %q;\n%s", n, got)
				}
			}
		})
	}
	// Both names come before the adapter, and a missing file is the default config.
	if got := strings.Join(installHints(config.Default(), path), ""); got != "" {
		t.Errorf("default config: %q", got)
	}
}

func TestInstallHintsCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	env := func(string) string { return "" }
	var out, errb bytes.Buffer

	// A missing file is the default config: nothing to say.
	if code := Main([]string{"install-hints", "--config", path}, env, &out, &errb); code != exitOK || out.Len() > 0 || errb.Len() > 0 {
		t.Errorf("missing file: code %d, out %q, err %q", code, out.String(), errb.String())
	}
	// A config aq cannot read is said on stderr; the installer goes on, as the
	// daemon, not the installer, is what refuses it.
	os.WriteFile(path, []byte("[stats]\nagent_process = [\"claude\"]\n"), 0o600)
	out.Reset()
	errb.Reset()
	if code := Main([]string{"install-hints", "--config", path}, env, &out, &errb); code != exitOK {
		t.Errorf("unreadable config: code %d, want %d", code, exitOK)
	}
	if e := errb.String(); !strings.HasPrefix(e, "warning: config "+path+": unknown keys: stats.agent_process") {
		t.Errorf("stderr = %q", e)
	}
	if out.Len() > 0 {
		t.Errorf("unreadable config printed hints: %q", out.String())
	}
	// The hints go to stdout with the path the installer named.
	os.WriteFile(path, []byte("[stats]\nagent_processes = [\"claude\", \"codex\"]\n"), 0o600)
	out.Reset()
	errb.Reset()
	if code := Main([]string{"install-hints", "--config", path}, env, &out, &errb); code != exitOK {
		t.Errorf("code %d, want %d", code, exitOK)
	}
	if o, e := out.String(), errb.String(); !strings.HasPrefix(o, "hint: agent_processes in "+path+" does not list openclaude.\n") || e != "" {
		t.Errorf("out %q, err %q", o, e)
	}
	// The file to read is named, not guessed.
	out.Reset()
	if code := Main([]string{"install-hints"}, env, &out, &errb); code != exitUsage {
		t.Errorf("without --config: code %d, want %d", code, exitUsage)
	}
}
