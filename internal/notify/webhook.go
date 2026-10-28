package notify

import (
	"bytes"
	"context"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"io"
	"net/http"
	"os"
	"strings"
)

type webhookDeliverer struct {
	cfg    config.WebhookConfig
	client *http.Client
}

func NewWebhookDeliverer(w config.WebhookConfig, client *http.Client) Deliverer {
	if client == nil {
		// Only a 2xx from the configured URL is a delivery: a followed redirect turns the
		// POST into a body-less GET whose 200 (say, a login page) would mark the event sent.
		client = &http.Client{Timeout: w.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &webhookDeliverer{w, client}
}
func (d *webhookDeliverer) Deliver(ctx context.Context, m Message) error {
	body, err := WebhookBody(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.Event.ID)
	if d.cfg.TokenFile != "" {
		b, err := os.ReadFile(d.cfg.TokenFile)
		if err != nil {
			return fmt.Errorf("webhook token_file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return err
}
