package usecase

import (
	"context"

	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// listTriggersOutput is the projection of a trigger for the model: readable,
// excludes internal IDs and secrets, keeps the cron and ingress address.
type listTriggersOutput struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Kind           string `json:"kind"` // schedule | webhook
	Enabled        bool   `json:"enabled"`
	Cron           string `json:"cron,omitempty"`
	PromptTemplate string `json:"prompt_template,omitempty"`
	WebhookToken   string `json:"webhook_token,omitempty"`
}

// listTriggers lets an agent discover its own recurring schedule and inbound
// webhooks — answering "when do I run next, and what is my canned prompt?"
// without hardcoding. Scoped by the agent's ScopeID, falling back to the
// project's default agent.
func listTriggers() opcore.Operation[struct{}, []listTriggersOutput] {
	return opcore.Operation[struct{}, []listTriggersOutput]{
		Name:    "list_triggers",
		Summary: "List this agent's configured schedules and webhooks. Discover when you run unattended and with what canned prompt.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, _ struct{}) ([]listTriggersOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return nil, err
			}
			scopeID := cc.MemoryScope()
			triggers, err := d.Repo.ListAgentTriggersForScope(ctx, scopeID)
			if err != nil {
				return nil, err
			}
			out := make([]listTriggersOutput, len(triggers))
			for i, t := range triggers {
				out[i] = listTriggersOutput{
					ID:             t.ID,
					Name:           t.Name,
					Kind:           string(t.Kind),
					Enabled:        t.Enabled,
					Cron:           t.Cron,
					PromptTemplate: t.PromptTemplate,
					WebhookToken:   t.WebhookToken,
				}
			}
			return out, nil
		},
	}
}
