// Package alerting evaluates saved alert rules and delivers firing/recovery
// notifications to configured channels. It is the platform-side worker for the
// Alerting feature (#1): condition storage lives in internal/storage, this
// package is the evaluation loop (driven off the agent scheduler tick) plus the
// delivery fan-out. The delivery half is also reused by the send_notification
// agent tool so an agent can post to the same channels it would alert on.
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/credential"
	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/internal/storage"
)

// Notification is one message to deliver. Title/Body are rendered per channel
// kind (Slack blocks vs a JSON webhook envelope).
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Level is informational metadata forwarded in the webhook envelope
	// ("firing" | "ok" | "info").
	Level string `json:"level"`
	// URL optionally deep-links back to the rule/dashboard.
	URL string `json:"url,omitempty"`
}

// Deliverer posts notifications to channels over an SSRF-guarded client and
// resolves {{cred:NAME}} placeholders in channel config against a vault at the
// trust boundary (secrets never sit in the DB in plaintext).
type Deliverer struct {
	client *http.Client
	vault  *credential.Vault
}

// NewDeliverer builds a deliverer. A nil vault disables secret resolution
// (config is used verbatim), which is the correct behavior when no host
// credentials are configured.
func NewDeliverer(vault *credential.Vault) *Deliverer {
	return &Deliverer{
		client: httptool.NewGuardedClient(15 * time.Second),
		vault:  vault,
	}
}

type slackConfig struct {
	WebhookURL string `json:"webhook_url"`
}

type webhookConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type emailConfig struct {
	// WebhookURL is a relay endpoint (e.g. an email-sending service's inbound
	// webhook) the platform POSTs to. Direct SMTP is intentionally out of scope
	// for v1 — a relay keeps delivery on the one guarded HTTP path.
	WebhookURL string `json:"webhook_url"`
	To         string `json:"to"`
}

// Deliver sends n to one channel. It resolves any {{cred:NAME}} in the channel
// config first, then dispatches by kind. Returns a descriptive error the caller
// records on the alert_event / surfaces to the send_notification tool.
func (d *Deliverer) Deliver(ctx context.Context, ch storage.AlertChannel, n Notification) error {
	cfg, err := d.resolveConfig(ctx, ch.Config)
	if err != nil {
		return err
	}
	switch ch.Kind {
	case "slack":
		return d.deliverSlack(ctx, cfg, n)
	case "webhook":
		return d.deliverWebhook(ctx, cfg, n)
	case "email":
		return d.deliverEmail(ctx, cfg, n)
	default:
		return fmt.Errorf("alerting: unknown channel kind %q", ch.Kind)
	}
}

// Notify is the send_notification adapter: an agent-driven message to one channel.
// It satisfies usecase.Notifier so opcore's send_notification operation can reach
// the same delivery fan-out the alert worker uses. Level is "info" (agent-initiated,
// not a threshold breach).
func (d *Deliverer) Notify(ctx context.Context, ch storage.AlertChannel, title, body string) error {
	return d.Deliver(ctx, ch, Notification{Title: title, Body: body, Level: "info"})
}

// resolveConfig substitutes {{cred:NAME}} placeholders in the raw channel config
// JSON. Done once, on the whole JSON blob, before it is parsed — so a secret can
// appear anywhere (url, header value).
func (d *Deliverer) resolveConfig(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if d.vault == nil || len(raw) == 0 {
		return raw, nil
	}
	resolved, err := d.vault.Resolve(ctx, string(raw))
	if err != nil {
		return nil, fmt.Errorf("alerting: resolving channel secrets: %w", err)
	}
	return json.RawMessage(resolved), nil
}

func (d *Deliverer) deliverSlack(ctx context.Context, cfg json.RawMessage, n Notification) error {
	var c slackConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return fmt.Errorf("alerting: bad slack config: %w", err)
	}
	if strings.TrimSpace(c.WebhookURL) == "" {
		return fmt.Errorf("alerting: slack channel missing webhook_url")
	}
	text := n.Title
	if n.Body != "" {
		text += "\n" + n.Body
	}
	if n.URL != "" {
		text += "\n" + n.URL
	}
	payload, _ := json.Marshal(map[string]string{"text": text})
	return d.post(ctx, c.WebhookURL, nil, payload)
}

func (d *Deliverer) deliverWebhook(ctx context.Context, cfg json.RawMessage, n Notification) error {
	var c webhookConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return fmt.Errorf("alerting: bad webhook config: %w", err)
	}
	if strings.TrimSpace(c.URL) == "" {
		return fmt.Errorf("alerting: webhook channel missing url")
	}
	payload, _ := json.Marshal(n)
	return d.post(ctx, c.URL, c.Headers, payload)
}

func (d *Deliverer) deliverEmail(ctx context.Context, cfg json.RawMessage, n Notification) error {
	var c emailConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return fmt.Errorf("alerting: bad email config: %w", err)
	}
	if strings.TrimSpace(c.WebhookURL) == "" {
		return fmt.Errorf("alerting: email channel missing webhook_url (v1 delivers email via a relay endpoint)")
	}
	payload, _ := json.Marshal(map[string]any{
		"to":      c.To,
		"subject": n.Title,
		"text":    n.Body,
		"level":   n.Level,
		"url":     n.URL,
	})
	return d.post(ctx, c.WebhookURL, nil, payload)
}

func (d *Deliverer) post(ctx context.Context, url string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerting: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("alerting: delivery failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alerting: delivery returned status %d", resp.StatusCode)
	}
	return nil
}
