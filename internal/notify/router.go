// Package notify routes and delivers durable notifications outside scheduler locks.
package notify

import (
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"github.com/creatorofuniverses/agent-queue/internal/model"
)

type Router struct{ cfg config.Config }

func NewRouter(cfg config.Config) *Router { return &Router{cfg: cfg} }
func (r *Router) Route(clientType string, aud model.Audience) model.Channel {
	if aud == model.AudienceOperator {
		if r.cfg.Webhook.URL != "" {
			return model.ChannelWebhook
		}
		return model.ChannelNone
	}
	if aud == model.AudienceSession {
		if a, ok := r.cfg.Agent(clientType); ok {
			if a.Delivery == config.DeliveryPush {
				return model.ExecChannel(a.Name)
			}
			if a.Delivery == config.DeliveryClientWait {
				return model.ChannelClientWait
			}
		}
	}
	return model.ChannelNone
}
func (r *Router) CheckClient(clientType, sessionID string, noNotify bool, lookPath func(string) (string, error)) error {
	if clientType == "" || clientType == "cli" {
		return nil
	}
	a, ok := r.cfg.Agent(clientType)
	if !ok {
		return fmt.Errorf("unknown client %s", clientType)
	}
	if a.Delivery == config.DeliveryPush && sessionID != "" && !noNotify {
		if _, err := lookPath(a.Command[0]); err != nil {
			return fmt.Errorf("cannot deliver to %s: %s not found in daemon PATH", a.Name, a.Command[0])
		}
	}
	return nil
}
