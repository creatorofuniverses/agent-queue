package client

import (
	"fmt"
	"resource-dispatcher/internal/config"
)

type Identity struct{ ClientType, SessionID string }

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
					return Identity{}, &UsageError{fmt.Sprintf("--client %s needs a session (--session or %s)", flagClient, a.IdentityEnv)}
				}
				break
			}
		}
		if !found {
			return Identity{}, &UsageError{fmt.Sprintf("unknown client %s", flagClient)}
		}
		return id, nil
	}
	for _, a := range agents {
		if s := env(a.IdentityEnv); s != "" {
			id.ClientType = a.Name
			if flagSession == "" {
				id.SessionID = s
			}
			return id, nil
		}
	}
	return id, nil
}
