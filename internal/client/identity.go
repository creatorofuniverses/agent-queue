package client

import (
	"errors"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"strings"
)

type Identity struct{ ClientType, SessionID string }

// ErrAmbiguousIdentity: identity variables of several adapters are set. Children inherit
// them, so a Claude session started from a Codex shell still carries CODEX_THREAD_ID and
// picking either one could wake the wrong session. Under an agent ancestor only several
// own variables are ambiguous: those the agent's start environment does not hold.
var ErrAmbiguousIdentity = errors.New("ambiguous session identity")

func DetectIdentity(parent Parent, agents []config.AgentConfig, env func(string) string, flagClient, flagSession string) (Identity, error) {
	id := Identity{ClientType: "cli", SessionID: flagSession}
	if flagClient != "" {
		id.ClientType = flagClient
		if flagClient == "cli" {
			return id, nil
		}
		found := false
		for _, a := range agents {
			if a.Name == flagClient {
				found = true
				if id.SessionID == "" {
					id.SessionID = env(a.IdentityEnv)
				}
				if a.Delivery == config.DeliveryPush && id.SessionID == "" {
					return Identity{}, &UsageError{Message: fmt.Sprintf("--client %s needs a session (--session or %s)", flagClient, a.IdentityEnv)}
				}
				if isCodex(a) {
					id.SessionID = codexRootThread(env, id.SessionID)
				}
				break
			}
		}
		if !found {
			return Identity{}, &UsageError{Message: fmt.Sprintf("unknown client %s", flagClient)}
		}
		return id, nil
	}
	// The nearest agent ancestor decides; the environment alone only without one.
	if pid, ok := parent.nearestAgent(); ok {
		return parent.identity(pid, agents, env, flagSession)
	}
	var found []config.AgentConfig
	for _, a := range agents {
		if env(a.IdentityEnv) != "" {
			found = append(found, a)
		}
	}
	if len(found) > 1 {
		return Identity{}, ambiguous(found)
	}
	if len(found) == 1 {
		id.ClientType = found[0].Name
		if flagSession == "" {
			id.SessionID = env(found[0].IdentityEnv)
		}
		if isCodex(found[0]) {
			id.SessionID = codexRootThread(env, id.SessionID)
		}
	}
	return id, nil
}

// ambiguous: identity variables of several adapters are set in this environment.
func ambiguous(found []config.AgentConfig) error {
	vars := make([]string, len(found))
	for i, a := range found {
		vars[i] = a.IdentityEnv
	}
	return &UsageError{Message: fmt.Sprintf("%s are all set (a session started from another agent's shell inherits its variable); pass --client NAME and --session ID of your own session", strings.Join(vars, ", ")), Err: ErrAmbiguousIdentity}
}
