'use client';

import { useState } from 'react';
import { useParams, useRouter } from 'next/navigation';
import { ArrowLeft, Clock, Copy, Cpu, ShieldCheck, Sparkles, Trash2, UserCog, Users, Webhook, Wrench, Zap } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { TextArea } from '@astryxdesign/core/TextArea';
import { Selector } from '@astryxdesign/core/Selector';
import { AGENT_TASK_KINDS, type AgentTaskKind, type AgentTaskTiers, type AgentTrigger, type AgentTriggerInput, apiBase, type BudgetStatus, MODEL_TIERS, type ModelTier } from '@/lib/api';
import { useAgent, useAgentAuthoring, useAgentBudget, useAgentBuild, useAgentCapabilities, useAgentTaskTiers } from '@/modules/agent/hooks';
import { useAgentMonitorDetail } from '@/modules/agent-monitor/hooks';
import { useUIStore } from '@/lib/app-state';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro, Loading, Panel, Segment } from '@/modules/shared/components/signal-primitives';

// The per-agent setup surface (DESIGN: AgentGarden — zero backend code per agent).
// Every section here drives an API that already existed but had no UI: the agent's
// persona/instructions (definition), which tools it may call, what it's permitted
// to touch (capability scopes), and which model tier runs each kind of work. The
// language is deliberately plain so a non-technical product owner can configure a
// teammate without knowing the underlying model/tool plumbing.
//
// Astryx migration: natural-language fields use Astryx TextInput/TextArea/Selector.
// The remaining bespoke `inputCls`/`textareaCls` controls are deliberate exceptions —
// genuine monospace code fields (JSON tool config, cron expressions, the read-only
// webhook URL) — since Astryx TextArea/TextInput can't set a monospace control font
// through their typed props (same exception class as the SQL editor).
const inputCls =
  'h-9 w-full rounded-md border border-[var(--color-border-emphasized)] bg-[var(--color-background-muted)] px-3 text-[13px] text-[var(--color-text-primary)] outline-none focus:border-primary focus:shadow-[0_0_0_3px_var(--ring)]';
const labelCls = 'mb-1.5 block text-[12.5px] text-[var(--color-text-secondary)]';
const textareaCls =
  'min-h-[180px] w-full rounded-md border border-[var(--color-border-emphasized)] bg-[var(--color-background-muted)] p-3 font-mono text-[12.5px] leading-[1.6] text-[var(--color-text-primary)] outline-none focus:border-primary focus:shadow-[0_0_0_3px_var(--ring)]';

// Toggle is a plain on/off switch styled like the rest of the surface, used for
// tools and permissions so the page reads as a list of capabilities a teammate
// can be trusted with.
function Toggle({ on, onClick, disabled }: { on: boolean; onClick: () => void; disabled?: boolean }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      disabled={disabled}
      onClick={onClick}
      className={`relative h-[22px] w-[38px] flex-none rounded-full transition-colors duration-[var(--fast)] ${on ? 'bg-[var(--agent)]' : 'bg-[var(--color-background-muted)]'} disabled:opacity-50`}
    >
      <span className={`absolute top-[3px] h-4 w-4 rounded-full bg-white transition-[left] duration-[var(--fast)] ${on ? 'left-[19px]' : 'left-[3px]'}`} />
    </button>
  );
}

// --- Persona: the agent's role + working instructions, with an optional AI draft.
function PersonaTab({ agentID }: { agentID: string }) {
  const { definition, definitionLoading, definitionDraftPending, saveDefinition, generateDefinitionDraft } = useAgentAuthoring(agentID);
  const [soul, setSoul] = useState<string | null>(null);
  const [agents, setAgents] = useState<string | null>(null);
  const [seeded, setSeeded] = useState<unknown>(null);
  const [idea, setIdea] = useState('');
  const [saving, setSaving] = useState(false);

  // Seed the editors off the loaded definition (and re-seed after a save
  // invalidation), the same render-time pattern the models tab uses.
  if (definition && definition !== seeded) {
    setSeeded(definition);
    setSoul(definition.soul_md);
    setAgents(definition.agents_md);
  }

  if (definitionLoading && soul === null) return <Panel title="Persona"><Loading label="Loading persona…" /></Panel>;

  const onDraft = async () => {
    if (!idea.trim()) return;
    const draft = await generateDefinitionDraft(idea.trim());
    setSoul(draft.soul_md);
    setAgents(draft.agents_md);
  };
  const onSave = async () => {
    setSaving(true);
    try { await saveDefinition(soul ?? '', agents ?? ''); } finally { setSaving(false); }
  };

  return (
    <div className="flex flex-col gap-[14px]">
      <Panel title="Describe the teammate" action={<Button variant="agent" size="sm" icon={<Sparkles size={14} />} onClick={() => void onDraft()} disabled={definitionDraftPending || !idea.trim()}>{definitionDraftPending ? 'Drafting…' : 'Draft with AI'}</Button>}>
        <p className="mb-2 max-w-[620px] text-[12px] text-[var(--color-text-secondary)]">In one line, say what this agent is for. We&apos;ll draft a starting persona and instructions you can edit below.</p>
        <TextInput label="What is this agent for?" isLabelHidden value={idea} placeholder="e.g. A growth analyst who watches signups and flags drops" onChange={(v) => setIdea(v)} width="100%" />
      </Panel>

      <Panel title="Personality & role">
        <p className="mb-2 max-w-[620px] text-[12px] text-[var(--color-text-secondary)]">Who the agent is and how it should talk — its voice, priorities, and what it cares about.</p>
        <TextArea label="Personality & role" isLabelHidden rows={9} width="100%" value={soul ?? ''} onChange={(v) => setSoul(v)} placeholder="You are a friendly growth analyst…" />
      </Panel>

      <Panel title="Working instructions">
        <p className="mb-2 max-w-[620px] text-[12px] text-[var(--color-text-secondary)]">How it should do the work — steps to follow, things to always check, and what to avoid.</p>
        <TextArea label="Working instructions" isLabelHidden rows={9} width="100%" value={agents ?? ''} onChange={(v) => setAgents(v)} placeholder="When asked about a metric, always…" />
      </Panel>

      <div><Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>{saving ? 'Saving…' : 'Save persona'}</Button></div>
    </div>
  );
}

// --- Tools: which capabilities the agent is allowed to use. Configurable tools
// (e.g. web requests) take a small JSON config; the server validates it on enable.
function ToolsTab({ agentID }: { agentID: string }) {
  const { catalog, selections, toolsLoading, setTool, clearTool } = useAgentBuild(agentID);
  const [configs, setConfigs] = useState<Record<string, string>>({});

  if (toolsLoading && catalog.length === 0) return <Panel title="Tools"><Loading label="Loading tools…" /></Panel>;

  const selByName = new Map(selections.map((s) => [s.name, s]));
  const configFor = (name: string) => configs[name] ?? selByName.get(name)?.config ?? '{}';

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">Turn on only what this teammate needs. Each tool is a thing the agent can do on your behalf — keeping the list tight keeps it predictable.</p>
      {catalog.map((tool) => {
        const sel = selByName.get(tool.name);
        const on = !!sel?.enabled;
        return (
          <Panel key={tool.name} title={tool.title || tool.name} action={
            <Toggle on={on} onClick={() => {
              if (on) void clearTool(tool.name);
              else void setTool(tool.name, true, JSON.parse(tool.configurable ? configFor(tool.name) : '{}'));
            }} />
          }>
            <p className="max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">{tool.description}</p>
            {tool.configurable ? (
              <div className="mt-3">
                <label className={labelCls}>Settings <span className="text-[var(--color-text-disabled)]">(JSON — e.g. allowed hosts)</span></label>
                <textarea
                  className={`${textareaCls} min-h-[80px]`}
                  value={configFor(tool.name)}
                  onChange={(e) => setConfigs((c) => ({ ...c, [tool.name]: e.target.value }))}
                />
                <div className="mt-2">
                  <Button variant="outline" size="sm" onClick={() => {
                    try { void setTool(tool.name, true, JSON.parse(configFor(tool.name))); }
                    catch { /* invalid JSON — the server-side validation toast covers the enable path */ }
                  }}>Save settings</Button>
                </div>
              </div>
            ) : null}
          </Panel>
        );
      })}
      {catalog.length === 0 ? <EmptyState icon={<Wrench size={20} />} title="No tools available" detail="This workspace has no selectable tools yet." /> : null}
    </div>
  );
}

// --- Teammates: which other agents this one may hand a task to. The grant
// backs spawn_subagent's agent parameter: the delegate runs under its OWN
// persona, tools, and permissions — nothing of this agent's access leaks
// across. Self-delegation (forking a clone of itself) is always available and
// doesn't need a grant.
function TeammatesTab({ agentID }: { agentID: string }) {
  const { delegateAgents, delegateSelections, delegatesLoading, setDelegate, clearDelegate } = useAgentBuild(agentID);

  if (delegatesLoading && delegateAgents.length === 0) return <Panel title="Teammates"><Loading label="Loading teammates…" /></Panel>;

  const enabledByID = new Map(delegateSelections.map((s) => [s.agent_id, s.enabled]));
  const candidates = delegateAgents.filter((a) => a.id !== agentID);

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">
        Let this agent hand tasks to other agents. A teammate does the delegated task with its <em>own</em> persona, tools, and permissions, then reports back only its final answer. This agent can always delegate to a copy of itself — no setting needed.
      </p>
      {candidates.map((a) => {
        const on = enabledByID.get(a.id) === true;
        return (
          <Panel key={a.id} title={a.name} action={
            <Toggle on={on} disabled={!a.enabled} onClick={() => {
              if (on) void clearDelegate(a.id);
              else void setDelegate(a.id, true);
            }} />
          }>
            <p className="max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">
              {a.enabled ? <>Allow handing tasks to <span className="font-medium text-[var(--color-text-primary)]">{a.name}</span> ({a.slug}).</> : 'This agent is currently disabled and cannot receive tasks.'}
            </p>
          </Panel>
        );
      })}
      {candidates.length === 0 ? <EmptyState icon={<Users size={20} />} title="No other agents yet" detail="Create another agent in this project to enable delegation between them." /> : null}
    </div>
  );
}

// --- Permissions: the capability scopes that gate what kinds of work the agent
// may do. Default-deny; the owner opts in per area.
const SCOPES: Array<{ id: string; label: string; detail: string }> = [
  { id: 'monitor', label: 'Watch the numbers', detail: 'Read metrics, traffic, and events to keep an eye on the product.' },
  { id: 'data_quality', label: 'Check data quality', detail: 'Spot gaps, anomalies, and broken tracking in your data.' },
  { id: 'analyze_build', label: 'Analyze & build', detail: 'Run deeper analysis and build charts or dashboards.' },
  { id: 'growth_suggest', label: 'Suggest growth ideas', detail: 'Propose experiments and growth recommendations.' },
];

function PermissionsTab({ agentID }: { agentID: string }) {
  const { capabilities, capabilitiesLoading, saveCapabilities } = useAgentCapabilities(agentID);
  const [draft, setDraft] = useState<Record<string, boolean> | null>(null);
  const [seeded, setSeeded] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  if (capabilities && capabilities !== seeded) {
    setSeeded(capabilities);
    setDraft({ ...capabilities.scopes });
  }
  if (capabilitiesLoading && !draft) return <Panel title="Permissions"><Loading label="Loading permissions…" /></Panel>;
  const scopes = draft ?? {};

  const onSave = async () => {
    setSaving(true);
    try { await saveCapabilities(scopes); } finally { setSaving(false); }
  };

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">Decide what this teammate is trusted to do. Everything is off until you allow it.</p>
      {SCOPES.map((s) => (
        <Panel key={s.id} title={s.label} action={<Toggle on={!!scopes[s.id]} onClick={() => setDraft((d) => ({ ...(d ?? {}), [s.id]: !(d?.[s.id]) }))} />}>
          <p className="max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">{s.detail}</p>
        </Panel>
      ))}
      <div><Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>{saving ? 'Saving…' : 'Save permissions'}</Button></div>
      <AutonomySection />
    </div>
  );
}

// --- Autonomy: how much this project's agents may do with no human watching.
// Project-level (agent_configs.autonomy) and enforced by the runner, not by
// persona text: below "Auto", catalog tools flagged external-write (currently
// Fetch / HTTP) are stripped from every unattended run — schedules, webhooks,
// and delegated hand-offs. Sandbox browsing tools are not flagged; for agents
// granted those, instructions and host allowlists are the guard.
const AUTONOMY_RUNGS = [
  {
    value: 'suggest',
    label: 'Suggest',
    detail: 'The default. Agents analyze, draft, and file recommendations. Runs that start without a human (schedules, webhooks, delegated tasks) never carry the Fetch / HTTP publishing tools.',
  },
  {
    value: 'scheduled',
    label: 'Scheduled',
    detail: 'Agents work unattended on their triggers, but the Fetch / HTTP publishing tools are still removed from those runs — unattended work stops at a draft for human review.',
  },
  {
    value: 'auto',
    label: 'Auto — can publish unattended',
    detail: 'Unattended runs keep Fetch / HTTP (to the hosts you allow). An agent can post to external services with no human reviewing the exact content first.',
  },
];
const AUTONOMY_OPTIONS = AUTONOMY_RUNGS.map((r) => ({ value: r.value, label: r.label }));

function AutonomySection() {
  const { config, configLoading, saveConfig } = useAgent();
  const [value, setValue] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  if (configLoading && !config) return <Panel title="Autonomy"><Loading label="Loading autonomy…" /></Panel>;
  // A stored value outside the ladder (legacy row, manual DB edit) is treated
  // as Suggest — which is exactly how the runner's fail-closed rail treats it —
  // and surfaced so saving normalizes it instead of silently masking it.
  const stored = config?.autonomy || 'suggest';
  const known = AUTONOMY_RUNGS.some((r) => r.value === stored);
  const current = known ? stored : 'suggest';
  const selected = value ?? current;
  const rung = AUTONOMY_RUNGS.find((r) => r.value === selected) ?? AUTONOMY_RUNGS[0];

  const onSave = async () => {
    if (!config) return;
    setSaving(true);
    try {
      await saveConfig({ enabled: config.enabled, redact_pii: config.redact_pii, autonomy: selected, schedule_cron: config.schedule_cron });
    } finally { setSaving(false); }
  };

  return (
    <Panel title="Autonomy">
      <p className="mb-2.5 max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">
        How much agents in this project may do when no one is watching. This applies to every agent in the
        project and is enforced by the platform — below “Auto”, the Fetch / HTTP publishing tools are removed
        from every unattended run, whatever the agent&apos;s instructions say.
      </p>
      {!known ? (
        <p className="mb-2 max-w-[600px] text-[12px] font-medium" style={{ color: 'var(--color-warning)' }}>
          The stored autonomy value “{stored}” is not recognized, so agents run at Suggest (the strictest
          rung). Save to normalize it.
        </p>
      ) : null}
      <Segment options={AUTONOMY_OPTIONS} value={selected} onChange={setValue} />
      <p className="mt-2.5 max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">{rung.detail}</p>
      {selected === 'auto' ? (
        <p className="mt-2 max-w-[600px] text-[12px] font-medium" style={{ color: 'var(--color-warning)' }}>
          Only turn this on when you trust the agents and their allowed hosts more than a per-post review.
          Nobody approves a post beforehand, and the audit trail is filed by the agent itself — it is a duty
          in its instructions, not something the platform can guarantee.
        </p>
      ) : null}
      <div className="mt-3">
        <Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving || (known && selected === stored)}>
          {saving ? 'Saving…' : 'Save autonomy'}
        </Button>
      </div>
    </Panel>
  );
}

// --- Model: which workspace model tier handles each kind of work. Tiers are
// configured once in workspace settings; here you just pick speed vs. depth per task.
const TASK_LABELS: Record<AgentTaskKind, { label: string; detail: string }> = {
  triage: { label: 'Quick routing', detail: 'Deciding what a message needs — fast and cheap is fine.' },
  run: { label: 'Doing the work', detail: 'The main analysis and answers.' },
  compaction: { label: 'Summarizing history', detail: 'Compressing long conversations to save cost.' },
  reflection: { label: 'Learning & review', detail: 'Reflecting on past runs to improve.' },
};
const TIER_LABELS: Record<ModelTier, string> = { lite: 'Lite (fastest, cheapest)', flash: 'Default (balanced)', pro: 'Pro (deepest)' };

// BudgetBar surfaces the effective daily cap (#4c): a spend/cap progress bar plus
// an inline editor. A cap of 0 means "uncapped" on that dimension; the bar only
// renders for a set cost cap. When spend has tripped a cap the runner wraps the
// run up, so the exceeded state is shown prominently here.
function BudgetSection({ agentID }: { agentID: string }) {
  const { budgets, status, budgetLoading, saveBudget, clearBudget } = useAgentBudget(agentID);
  const dayRow = budgets.find((b) => b.period === 'day');
  const [cost, setCost] = useState<string | null>(null);
  const [seededFor, setSeededFor] = useState<string | null>(null);

  // Seed the editable field from the agent's own day cap once loaded (0 = blank).
  if (!budgetLoading && seededFor !== agentID) {
    setSeededFor(agentID);
    setCost(dayRow && dayRow.max_cost_usd > 0 ? String(dayRow.max_cost_usd) : '');
  }

  const save = () => {
    const parsed = Number(cost) || 0;
    saveBudget.mutate({ period: 'day', max_cost_usd: parsed, max_tokens: dayRow?.max_tokens ?? 0, max_runs: dayRow?.max_runs ?? 0 });
  };

  return (
    <Panel title="Daily budget">
      <p className="mb-3 max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">
        Cap what this teammate can spend per day. When the cap is reached it finishes its current
        thought and stops until tomorrow. Leave blank to inherit the workspace default (or run uncapped).
      </p>
      {budgetLoading && !status ? (
        <Loading label="Loading budget…" />
      ) : (
        <>
          <BudgetMeter status={status} />
          <div className="mt-3 flex items-end gap-2">
            <div>
              <label className={labelCls}>Max spend / day (USD)</label>
              <input
                className={`${inputCls} max-w-[180px]`}
                type="number"
                min="0"
                step="0.5"
                value={cost ?? ''}
                placeholder={status && status.has_budget && status.budget.is_workspace_default ? `${status.budget.max_cost_usd} (workspace default)` : 'Uncapped'}
                onChange={(e) => setCost(e.target.value)}
              />
            </div>
            <Button variant="primary" size="sm" onClick={save} disabled={saveBudget.isPending}>
              {saveBudget.isPending ? 'Saving…' : 'Save budget'}
            </Button>
            {dayRow ? (
              <Button variant="ghost" size="sm" onClick={() => { setCost(''); clearBudget.mutate('day'); }} disabled={clearBudget.isPending}>
                Clear
              </Button>
            ) : null}
          </div>
        </>
      )}
    </Panel>
  );
}

// BudgetMeter renders the spend-vs-cap bar for the resolved daily budget.
function BudgetMeter({ status }: { status: BudgetStatus | null }) {
  if (!status || !status.has_budget || status.budget.max_cost_usd <= 0) {
    return <p className="text-[12px] text-[var(--color-text-disabled)]">No spend cap — this agent runs uncapped.</p>;
  }
  const cap = status.budget.max_cost_usd;
  const spent = status.spend.cost_usd;
  const pct = Math.min(100, Math.round((spent / cap) * 100));
  const tone = status.exceeded ? 'var(--color-danger)' : pct >= 80 ? 'var(--color-warning)' : 'var(--agent)';
  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-[12px]">
        <span className="text-[var(--color-text-secondary)]">
          ${spent.toFixed(2)} of ${cap.toFixed(2)} today
          {status.budget.is_workspace_default ? ' (workspace default)' : ''}
        </span>
        <span style={{ color: tone }}>{status.exceeded ? `Capped — ${status.reason || 'budget exhausted'}` : `${pct}%`}</span>
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-[var(--color-background-muted)]">
        <div className="h-full rounded-full transition-[width]" style={{ width: `${pct}%`, backgroundColor: tone }} />
      </div>
    </div>
  );
}

function ModelTab({ agentID }: { agentID: string }) {
  const router = useRouter();
  const { taskTiers, taskTiersLoading, saveTaskTiers } = useAgentTaskTiers(agentID);
  const [draft, setDraft] = useState<AgentTaskTiers | null>(null);
  const [seeded, setSeeded] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  if (taskTiers && taskTiers !== seeded) {
    setSeeded(taskTiers);
    setDraft({ ...taskTiers });
  }
  if (taskTiersLoading && !draft) return <Panel title="Model"><Loading label="Loading model setup…" /></Panel>;
  const tiers = draft ?? {};

  const onSave = async () => {
    setSaving(true);
    try { await saveTaskTiers(tiers); } finally { setSaving(false); }
  };

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">Match the brainpower to the job. Use a lighter, cheaper model for quick steps and a stronger one where depth matters — a simple way to control cost. The actual models behind each tier are set in <button className="underline hover:text-[var(--color-text-primary)]" onClick={() => router.push('/settings')}>workspace settings</button>.</p>
      <BudgetSection agentID={agentID} />
      {AGENT_TASK_KINDS.map((kind) => (
        <Panel key={kind} title={TASK_LABELS[kind].label}>
          <p className="mb-3 max-w-[600px] text-[12px] text-[var(--color-text-secondary)]">{TASK_LABELS[kind].detail}</p>
          <Selector
            label="Model tier"
            isLabelHidden
            width={320}
            options={[{ value: '', label: 'Use default' }, ...MODEL_TIERS.map((t) => ({ value: t, label: TIER_LABELS[t] }))]}
            value={tiers[kind] ?? ''}
            onChange={(v) => setDraft((d) => ({ ...(d ?? {}), [kind]: (v || undefined) as ModelTier | undefined }))}
          />
        </Panel>
      ))}
      <div><Button variant="primary" size="sm" onClick={() => void onSave()} disabled={saving}>{saving ? 'Saving…' : 'Save model setup'}</Button></div>
    </div>
  );
}

// --- Triggers: what starts a run without someone typing in chat (AgentGarden §7).
// A schedule fires on a cron; a webhook gives you an unguessable URL that any
// system can POST to. Both reuse the same run path — this tab is the missing UI
// over an API that already existed. The HMAC secret (optional, webhook-only) names
// a vault secret used to authenticate inbound bodies via the X-Agent-Signature header.

const PROMPT_PLACEHOLDER = 'What should the agent do when this fires? Use {{body}} to drop in the webhook payload.';

// hookURL is the public ingress address of a webhook trigger: an unguessable
// per-trigger token in the path is the credential, so anyone holding the URL can
// fire the agent. Mirrors the backend route POST /api/agent/hook/:token.
const hookURL = (token: string) => `${apiBase()}/api/agent/hook/${token}`;

// SecretSelect picks the optional HMAC secret a webhook authenticates bodies with.
// The list comes from the agent's vault (same secrets the Tools tab manages); an
// empty value means "accept unsigned bodies — the token alone is the credential".
function SecretSelect({ value, secretNames, onChange }: { value: string; secretNames: string[]; onChange: (v: string) => void }) {
  return (
    <div>
      <label className={labelCls}>Verify body with secret <span className="text-[var(--color-text-disabled)]">(optional — webhook only)</span></label>
      <Selector
        label="Verify body with secret"
        isLabelHidden
        width={320}
        options={[{ value: '', label: 'No verification' }, ...secretNames.map((n) => ({ value: n, label: n }))]}
        value={value}
        onChange={(v) => onChange(v)}
      />
    </div>
  );
}

// TriggerRow is one existing trigger with inline editing. The kind and the webhook
// token are immutable (changing them would invalidate the address), so only the
// enabled flag, cron, prompt, and HMAC secret are editable here.
function TriggerRow({ trigger, secretNames, onSave, onDelete }: {
  trigger: AgentTrigger;
  secretNames: string[];
  onSave: (id: string, input: Omit<AgentTriggerInput, 'kind'>) => Promise<unknown>;
  onDelete: (id: string) => Promise<unknown>;
}) {
  const setMessage = useUIStore((s) => s.setMessage);
  const [enabled, setEnabled] = useState(trigger.enabled);
  const [cron, setCron] = useState(trigger.cron);
  const [prompt, setPrompt] = useState(trigger.prompt_template);
  const [secret, setSecret] = useState(trigger.hmac_secret_name);
  const [busy, setBusy] = useState(false);
  const isWebhook = trigger.kind === 'webhook';

  const save = async (next?: Partial<{ enabled: boolean }>) => {
    setBusy(true);
    try {
      await onSave(trigger.id, {
        enabled: next?.enabled ?? enabled,
        cron,
        prompt_template: prompt,
        hmac_secret_name: secret,
      });
    } finally { setBusy(false); }
  };
  const copy = async () => { await navigator.clipboard.writeText(hookURL(trigger.webhook_token)); setMessage('Webhook URL copied'); };

  return (
    <Panel
      title={isWebhook ? 'Webhook' : 'Schedule'}
      action={<Toggle on={enabled} disabled={busy} onClick={() => { const v = !enabled; setEnabled(v); void save({ enabled: v }); }} />}
    >
      <div className="flex flex-col gap-3">
        {isWebhook ? (
          <div>
            <label className={labelCls}>Webhook URL <span className="text-[var(--color-text-disabled)]">(POST here to fire — keep it secret)</span></label>
            <div className="flex items-center gap-2">
              <input className={`${inputCls} font-mono text-[12px]`} readOnly value={hookURL(trigger.webhook_token)} onFocus={(e) => e.currentTarget.select()} />
              <Button variant="outline" size="sm" icon={<Copy size={13} />} onClick={() => void copy()}>Copy</Button>
            </div>
          </div>
        ) : (
          <div>
            <label className={labelCls}>Cron schedule <span className="text-[var(--color-text-disabled)]">(minute hour day month weekday)</span></label>
            <input className={`${inputCls} max-w-[320px] font-mono`} value={cron} placeholder="0 9 * * 1" onChange={(e) => setCron(e.target.value)} />
          </div>
        )}
        <div>
          <label className={labelCls}>Prompt</label>
          <TextArea label="Prompt" isLabelHidden rows={4} width="100%" value={prompt} placeholder={PROMPT_PLACEHOLDER} onChange={(v) => setPrompt(v)} />
        </div>
        {isWebhook ? <SecretSelect value={secret} secretNames={secretNames} onChange={setSecret} /> : null}
        <div className="flex gap-2">
          <Button variant="primary" size="sm" disabled={busy} onClick={() => void save()}>{busy ? 'Saving…' : 'Save'}</Button>
          <Button variant="ghost" size="sm" icon={<Trash2 size={13} />} disabled={busy} onClick={() => void onDelete(trigger.id)}>Delete</Button>
        </div>
      </div>
    </Panel>
  );
}

function TriggersTab({ agentID }: { agentID: string }) {
  const { triggers, triggersLoading, secretNames, createTrigger, updateTrigger, deleteTrigger } = useAgentBuild(agentID);
  const [kind, setKind] = useState<'schedule' | 'webhook'>('schedule');
  const [cron, setCron] = useState('');
  const [prompt, setPrompt] = useState('');
  const [secret, setSecret] = useState('');
  const [creating, setCreating] = useState(false);

  const add = async () => {
    setCreating(true);
    try {
      await createTrigger({ kind, enabled: true, cron: kind === 'schedule' ? cron : '', prompt_template: prompt, hmac_secret_name: kind === 'webhook' ? secret : '' });
      setCron(''); setPrompt(''); setSecret('');
    } finally { setCreating(false); }
  };

  if (triggersLoading && triggers.length === 0) return <Panel title="Triggers"><Loading label="Loading triggers…" /></Panel>;

  const canAdd = kind === 'webhook' || cron.trim().length > 0;

  return (
    <div className="flex flex-col gap-[14px]">
      <p className="max-w-[640px] text-[12.5px] text-[var(--color-text-secondary)]">Let this teammate start work on its own — on a schedule, or whenever another system calls in. Without a trigger it only runs when someone messages it in chat.</p>

      <Panel title="Add a trigger">
        <div className="flex flex-col gap-3">
          <div>
            <label className={labelCls}>How should it start?</label>
            <Segment
              options={[{ value: 'schedule', label: 'On a schedule' }, { value: 'webhook', label: 'From a webhook' }]}
              value={kind}
              onChange={(v) => setKind(v as 'schedule' | 'webhook')}
            />
          </div>
          {kind === 'schedule' ? (
            <div>
              <label className={labelCls}>Cron schedule <span className="text-[var(--color-text-disabled)]">(minute hour day month weekday)</span></label>
              <input className={`${inputCls} max-w-[320px] font-mono`} value={cron} placeholder="0 9 * * 1" onChange={(e) => setCron(e.target.value)} />
            </div>
          ) : (
            <p className="flex items-center gap-1.5 text-[12px] text-[var(--color-text-secondary)]"><Webhook size={13} /> We&apos;ll generate a secret URL once you add this — you POST to it to fire the agent.</p>
          )}
          <div>
            <label className={labelCls}>Prompt</label>
            <TextArea label="Prompt" isLabelHidden rows={4} width="100%" value={prompt} placeholder={PROMPT_PLACEHOLDER} onChange={(v) => setPrompt(v)} />
          </div>
          {kind === 'webhook' ? <SecretSelect value={secret} secretNames={secretNames} onChange={setSecret} /> : null}
          <div><Button variant="primary" size="sm" icon={<Zap size={13} />} disabled={creating || !canAdd} onClick={() => void add()}>{creating ? 'Adding…' : 'Add trigger'}</Button></div>
        </div>
      </Panel>

      {triggers.length === 0 ? (
        <EmptyState icon={<Clock size={20} />} title="No triggers yet" detail="Add a schedule or webhook above to let this agent run on its own." />
      ) : (
        triggers.map((t) => (
          <TriggerRow key={t.id} trigger={t} secretNames={secretNames} onSave={updateTrigger} onDelete={deleteTrigger} />
        ))
      )}
    </div>
  );
}

const TABS = [
  { value: 'persona', label: 'Persona', icon: UserCog },
  { value: 'tools', label: 'Tools', icon: Wrench },
  { value: 'teammates', label: 'Teammates', icon: Users },
  { value: 'permissions', label: 'Permissions', icon: ShieldCheck },
  { value: 'triggers', label: 'Triggers', icon: Zap },
  { value: 'model', label: 'Model', icon: Cpu },
] as const;

export function AgentSetupPage() {
  const params = useParams<{ agentId: string }>();
  const router = useRouter();
  const agentID = params.agentId;
  const { agent, isLoading } = useAgentMonitorDetail(agentID);
  const [tab, setTab] = useState<string>('persona');

  if (isLoading && !agent) {
    return <AppShell active="agents"><Intro title="Agent setup" sub="Configure your teammate." /><Loading label="Loading agent…" /></AppShell>;
  }
  if (!agent) {
    return <AppShell active="agents"><Intro title="Agent setup" sub="Configure your teammate." /><EmptyState title="Agent not found" detail="This agent may have been removed." action={<Button variant="outline" size="sm" onClick={() => router.push('/agents')}>Back to agents</Button>} /></AppShell>;
  }

  return (
    <AppShell active="agents">
      <Intro
        title={<span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}><button className="flex-none grid h-[26px] w-[26px] place-items-center rounded-sm border-none bg-transparent text-[var(--color-text-secondary)] transition-[background,color] duration-[var(--fast)] ease-[var(--ease)] hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]" onClick={() => router.push('/agents')}><ArrowLeft size={15} /></button>{agent.name}</span>}
        sub="Set up how this teammate thinks, what it can use, and what it's trusted to do."
      />
      <div className="mb-3.5"><Segment options={TABS.map((t) => ({ value: t.value, label: t.label }))} value={tab} onChange={setTab} /></div>
      {tab === 'persona' ? <PersonaTab agentID={agentID} /> : null}
      {tab === 'tools' ? <ToolsTab agentID={agentID} /> : null}
      {tab === 'teammates' ? <TeammatesTab agentID={agentID} /> : null}
      {tab === 'permissions' ? <PermissionsTab agentID={agentID} /> : null}
      {tab === 'triggers' ? <TriggersTab agentID={agentID} /> : null}
      {tab === 'model' ? <ModelTab agentID={agentID} /> : null}
    </AppShell>
  );
}
