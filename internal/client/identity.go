package client

import (
	"agent-queue/internal/config"
	"errors"
	"fmt"
	"strings"
)

type Identity struct{ ClientType, SessionID string }

// ErrAmbiguousIdentity: identity variables of several adapters are set. Children inherit
// them, so a Claude session started from a Codex shell still carries CODEX_THREAD_ID and
// picking either one could wake the wrong session.
var ErrAmbiguousIdentity = errors.New("ambiguous session identity")

func DetectIdentity(agents []config.AgentConfig, env func(string) string, flagClient, flagSession string) (Identity, error) {
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
				break
			}
		}
		if !found {
			return Identity{}, &UsageError{Message: fmt.Sprintf("unknown client %s", flagClient)}
		}
		return id, nil
	}
	var found []config.AgentConfig
	for _, a := range agents {
		if env(a.IdentityEnv) != "" {
			found = append(found, a)
		}
	}
	if len(found) > 1 {
		vars := make([]string, len(found))
		for i, a := range found {
			vars[i] = a.IdentityEnv
		}
		return Identity{}, &UsageError{Message: fmt.Sprintf("%s are all set (a session started from another agent's shell inherits its variable); pass --client NAME and --session ID of your own session", strings.Join(vars, ", ")), Err: ErrAmbiguousIdentity}
	}
	if len(found) == 1 {
		id.ClientType = found[0].Name
		if flagSession == "" {
			id.SessionID = env(found[0].IdentityEnv)
		}
	}
	return id, nil
}
