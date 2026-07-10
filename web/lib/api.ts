export type Project = {
  id: string;
  workspace_id?: string;
  name: string;
  api_key: string;
  created_at: string;
};

export type User = {
  id: string;
  email: string;
  name: string;
  created_at: string;
  updated_at: string;
};

export type Workspace = {
  id: string;
  name: string;
  role: string;
  created_at: string;
  updated_at: string;
};

export type WorkspaceUsage = {
  workspace_id: string;
  project_count: number;
  event_count: number;
  distinct_users: number;
  generated_at: string;
};

export type WorkspaceRole = 'owner' | 'admin' | 'member';

export type WorkspaceMember = {
  workspace_id: string;
  user_id: string;
  email: string;
  name: string;
  role: WorkspaceRole;
  created_at: string;
};

export type WorkspaceAuditLog = {
  id: string;
  workspace_id: string;
  actor_id: string;
  actor_email: string;
  action: string;
  target_type: string;
  target_id: string;
  target_label: string;
  metadata: string;
  created_at: string;
};

export type AuthState = {
  user: User;
  session_expires_at: string;
  workspaces: Workspace[];
  projects: Project[];
  project: Project;
};

export type Event = {
  project_id: string;
  event_id: string;
  distinct_id: string;
  session_id: string;
  event_name: string;
  event_type: string;
  properties: string;
  is_error: boolean;
  agent_id?: string;
  tool_name?: string;
  tool_input?: string;
  tool_output?: string;
  tokens_input?: number;
  tokens_output?: number;
  cost_usd?: number;
  latency_ms?: number;
  model_name?: string;
  error_message?: string;
  timestamp: string;
  inserted_at?: string;
  is_unplanned?: boolean;
};

export type Session = {
  session_id: string;
  distinct_id: string;
  session_start: string;
  session_end: string;
  event_count: number;
  total_tokens_in: number;
  total_tokens_out: number;
  total_cost_usd: number;
};

export type ActivitySummary = {
  project_id: string;
  event_count: number;
  user_events: number;
  agent_events: number;
  system_events: number;
  sessions: number;
  distinct_users: number;
  total_tokens_in: number;
  total_tokens_out: number;
  total_cost_usd: number;
  event_counts: Array<{ event_name: string; count: number }>;
  timeline: Array<{ hour: string; count: number }>;
  top_agents: Array<{ agent_id: string; event_count: number; total_cost_usd: number; avg_latency_ms: number }>;
  recent_events: Event[];
  recent_sessions: Session[];
  events_by_type: Record<string, number>;
  generated_at: string;
};

export type Dashboard = {
  id: string;
  project_id: string;
  name: string;
  description: string;
  created_at: string;
  updated_at: string;
};

export type Chart = {
  id: string;
  dashboard_id: string;
  project_id: string;
  name: string;
  kind: 'line' | 'bar' | 'pie' | 'stat';
  metric: 'events' | 'event_breakdown' | 'tokens' | 'cost' | 'sessions';
  event_name: string;
  event_type: string;
  sql: string;
  x_field: string;
  y_field: string;
  sort_order: number;
  col_span: number;
  created_at: string;
  updated_at: string;
};

export type ChartInput = Pick<Chart, 'name' | 'kind' | 'metric' | 'event_name' | 'event_type' | 'sql' | 'x_field' | 'y_field' | 'col_span'>;

export type Filters = {
  hours: number;
  from: string;
  to: string;
  event_type: string;
  event_name: string;
  distinct_id: string;
  session_id: string;
  agent_id: string;
  model_name: string;
  search: string;
  error_only: boolean;
  limit: number;
};

export type InsightResult = {
  type: string;
  title: string;
  metric: string;
  series: Array<{ hour: string; count: number }>;
  rows: Array<Record<string, unknown>>;
  funnel: Array<{ step: number; event_name: string; users: number; conversion: number }>;
  retention: Array<{ period: string; users: number; rate: number }>;
  generated_at: string;
};

export type TemplateChart = {
  id: string;
  template_id: string;
  name: string;
  kind: Chart['kind'];
  metric: Chart['metric'];
  event_name: string;
  event_type: string;
  sql: string;
  x_field: string;
  y_field: string;
  sort_order: number;
};

export type DashboardTemplate = {
  id: string;
  project_id: string | null;
  name: string;
  description: string;
  is_system: boolean;
  charts: TemplateChart[];
};

export type AgentPresetSkill = {
  name: string;
  description: string;
  body: string;
};

export type AgentPreset = {
  slug: string;
  name: string;
  tagline: string;
  description: string;
  category: string;
  icon: string;
  scopes: Record<string, boolean>;
  skills: AgentPresetSkill[];
};

export type TrafficClass = {
  class: string;
  count: number;
};

export type TrafficProvider = {
  class: string;
  provider: string;
  visitors: number;
  pageviews: number;
};

export type GuestUser = {
  guests: number;
  users: number;
};

export type WebAnalytics = {
  visitors: number;
  pageviews: number;
  sessions: number;
  conversions: number;
  avg_session_duration_seconds: number;
  bounce_rate: number;
  top_paths: Array<{ value: string; count: number }>;
  referrers: Array<{ value: string; count: number }>;
  traffic_by_class: TrafficClass[];
  traffic_by_provider: TrafficProvider[];
  ai_top_paths: Array<{ value: string; count: number }>;
  referrers_by_channel: Array<{ value: string; count: number }>;
  guest_vs_user: GuestUser;
  generated_at: string;
};

export type Person = {
  distinct_id: string;
  email: string;
  name: string;
  first_seen: string;
  last_seen: string;
  event_count: number;
  sessions: number;
  last_event_name: string;
  // Merged $set / $set_once person profile traits (present only for identified
  // people with stored properties). Values are raw JSON — string, number, bool.
  traits?: Record<string, unknown>;
};

export type PersonsSummary = {
  total: number;
  identified: number;
  anonymous: number;
  active_timeline: Array<{ hour: string; count: number }>;
  persons: Person[];
  generated_at: string;
};

export type CohortCell = {
  period: number;
  users: number;
  rate: number;
};

export type CohortRow = {
  cohort: string;
  cohort_start: string;
  size: number;
  cells: CohortCell[];
};

// AudienceOption is one selectable cohort segment; the catalog is server-driven
// (see audienceSegments in store.go) so adding a group like paid/premium needs
// no client change.
export type AudienceOption = {
  key: string;
  label: string;
};

export type CohortAnalysis = {
  segment: string;
  periods: number;
  audiences: AudienceOption[];
  rows: CohortRow[];
  generated_at: string;
};

// AudienceKind is how a custom audience detects its members. Static-trait kinds
// are all-time aggregates: "paid" = ever fired a positive revenue event; "plan" =
// latest `plan` ∈ a configured set. Subscription-state kinds are point-in-time over
// the per-person subscription projection and require a configured SubscriptionMapping:
// "active_subscriber" = active or trialing now; "trialing"; "churned" = was active,
// now expired/cancelled; "plan_active" = active AND plan ∈ a set. Mirrors
// audienceKind* in store.go.
export type AudienceKind = 'paid' | 'plan' | 'active_subscriber' | 'trialing' | 'churned' | 'plan_active';

// SUBSCRIPTION_KINDS are the AudienceKinds backed by the subscription projection —
// they only resolve when the project has a configured SubscriptionMapping.
export const SUBSCRIPTION_KINDS: AudienceKind[] = ['active_subscriber', 'trialing', 'churned', 'plan_active'];

// SubscriptionMapping tells AgentRay which of a project's events/properties carry
// subscription lifecycle, so the cohort projection can derive point-in-time status
// (active/trialing/churned) per person. Token fields are validated server-side
// (^[A-Za-z0-9_.$:-]*$, ≤64) and escaped before SQL interpolation — never raw SQL.
// `configured` is false until the project saves a mapping (defaults are Stripe-shaped).
export type SubscriptionMapping = {
  project_id: string;
  start_event: string;
  renew_event: string;
  cancel_event: string;
  plan_prop: string;
  amount_prop: string;
  period_end_prop: string;
  trial_prop: string;
  grace_days: number;
  configured: boolean;
};

export type SubscriptionMappingInput = {
  start_event: string;
  renew_event: string;
  cancel_event: string;
  plan_prop: string;
  amount_prop: string;
  period_end_prop: string;
  trial_prop: string;
  grace_days: number;
};

// ProjectAudience is a user-defined cohort segment scoped to one project. The
// rule is structured (kind + plans), compiled to safe SQL on the server — there
// is no raw-SQL field by design.
export type ProjectAudience = {
  id: string;
  project_id: string;
  key: string;
  label: string;
  kind: AudienceKind;
  plans: string[];
  created_at: string;
};

export type AudienceInput = {
  label: string;
  kind: AudienceKind;
  plans: string[];
};

export type EventExplorer = {
  events: Event[];
  timeline: Event[];
  generated_at: string;
};

// One distinct event name in the project, used to feed the event-name
// autocomplete so people pick a known name instead of recalling it.
export type EventCatalogEntry = {
  event_name: string;
  event_type: string;
  count: number;
  last_seen: string;
};

export type AgentReplay = {
  session_id: string;
  distinct_id: string;
  event_count: number;
  total_tokens_in: number;
  total_tokens_out: number;
  total_cost_usd: number;
  events: Event[];
};

export type SavedQuery = {
  id: string;
  project_id: string;
  natural_language: string;
  generated_sql: string;
  verified: boolean;
  result_cache?: unknown;
  created_at: string;
};

export type SavedQueryResult = {
  query: SavedQuery;
  rows: Array<Record<string, unknown>>;
  generated_at: string;
};

// --- AI Agent (§8, §10, §14.10) -------------------------------------------

export type AgentScopes = Record<string, boolean>;

// AgentConfig carries the project-level run-eligibility fields. Model tiers are
// no longer here — they live in the workspace model pool (WorkspaceModelTiers).
export type AgentConfig = {
  project_id: string;
  enabled: boolean;
  redact_pii: boolean;
  scopes: AgentScopes;
  autonomy: string;
  schedule_cron: string;
};

export type AgentConfigInput = {
  enabled: boolean;
  redact_pii: boolean;
  scopes?: AgentScopes;
  autonomy: string;
  schedule_cron: string;
};

export type AgentCapabilityConfig = {
  scope_id: string;
  scopes: AgentScopes;
};

// AgentGrant is one workspace-agent → project assignment. The scopes cap what
// the agent may do in that project (a project-level permission ceiling).
export type AgentGrant = {
  agent_id: string;
  project_id: string;
  scopes: AgentScopes;
  created_at: string;
};

// WorkspaceModelTiers is the workspace-shared model pool: the 3 tiers (flash is
// the bare provider/model/base_url/has_key; lite/pro are additive and fall back
// to flash) every project and agent in the workspace draws from. Configured once
// per workspace; keys never returned — only the *_has_key presence flags.
export type WorkspaceModelTiers = {
  workspace_id: string;
  provider: string;
  model: string;
  base_url: string;
  has_key: boolean;
  lite_provider: string;
  lite_model: string;
  lite_base_url: string;
  lite_has_key: boolean;
  pro_provider: string;
  pro_model: string;
  pro_base_url: string;
  pro_has_key: boolean;
  model_fallback: boolean;
};

export type WorkspaceModelTiersInput = {
  provider: string;
  model: string;
  base_url: string;
  api_key?: string; // '' keeps the stored key, '-' clears it
  lite_provider?: string;
  lite_model?: string;
  lite_base_url?: string;
  lite_api_key?: string;
  pro_provider?: string;
  pro_model?: string;
  pro_base_url?: string;
  pro_api_key?: string;
  model_fallback?: boolean;
};

// --- Alerting (#1) ---

export const ALERT_SOURCE_KINDS = ['insight', 'sql', 'agent_ops'] as const;
export type AlertSourceKind = (typeof ALERT_SOURCE_KINDS)[number];
export const ALERT_OPS = ['gt', 'lt', 'z_score'] as const;
export type AlertOp = (typeof ALERT_OPS)[number];
export const ALERT_CHANNEL_KINDS = ['slack', 'email', 'webhook'] as const;
export type AlertChannelKind = (typeof ALERT_CHANNEL_KINDS)[number];

export type AlertCondition = {
  op: AlertOp;
  value: number;
  window?: number;
  min_events?: number;
};

export type AlertRule = {
  id: string;
  project_id: string;
  name: string;
  source_kind: AlertSourceKind;
  source_ref: string;
  condition: AlertCondition;
  schedule_cron: string;
  channels: string[];
  enabled: boolean;
  last_eval_at?: string;
  last_state?: string;
  created_at: string;
};

export type AlertRuleInput = {
  name: string;
  source_kind: AlertSourceKind;
  source_ref: string;
  condition: AlertCondition;
  schedule_cron: string;
  channels: string[];
  enabled: boolean;
};

export type AlertChannel = {
  id: string;
  workspace_id: string;
  kind: AlertChannelKind;
  name: string;
  created_at: string;
};

export type AlertChannelInput = {
  kind: AlertChannelKind;
  name: string;
  config: Record<string, unknown>;
};

export type AlertEvent = {
  id: string;
  rule_id: string;
  fired_at: string;
  state: string;
  value: number;
};

// --- Per-agent budgets & quotas (#4) ---

export type BudgetPeriod = 'day' | 'month';

export type AgentBudget = {
  scope_id: string;
  period: BudgetPeriod;
  max_cost_usd: number;
  max_tokens: number;
  max_runs: number;
  is_workspace_default: boolean;
  updated_at: string;
};

export type BudgetSpend = {
  period: BudgetPeriod;
  cost_usd: number;
  tokens: number;
  runs: number;
  since: string;
  as_of: string;
};

export type BudgetStatus = {
  budget: AgentBudget;
  spend: BudgetSpend;
  has_budget: boolean;
  exceeded: boolean;
  reason?: string;
};

export type AgentBudgetInput = {
  period: BudgetPeriod;
  max_cost_usd: number;
  max_tokens: number;
  max_runs: number;
};

// AGENT_TASK_KINDS are the 4 LLM call sites whose tier each agent maps. The map
// value is a workspace tier name ('lite' | 'flash' | 'pro').
export const AGENT_TASK_KINDS = ['triage', 'run', 'compaction', 'reflection'] as const;
export type AgentTaskKind = (typeof AGENT_TASK_KINDS)[number];

// AgentTaskTiers maps each task kind to the workspace tier it runs on. A partial
// map still resolves all 4 (the server merges over the default).
export type AgentTaskTiers = Partial<Record<AgentTaskKind, ModelTier>>;

// AgentConfigTestResult is the per-tier connectivity check returned by
// testWorkspaceModels: only configured tiers appear in `tiers`.
export type AgentConfigTestResult = {
  ok: boolean;
  tiers?: Record<string, { ok: boolean; error?: string }>;
};

// MODEL_TIERS is the fixed low→high escalation order the agent walks on a
// retryable provider error.
export const MODEL_TIERS = ['lite', 'flash', 'pro'] as const;
export type ModelTier = (typeof MODEL_TIERS)[number];

// MODEL_SUGGESTIONS maps a provider to a per-tier default model plus the
// suggestion list shown in the picker. Free text is always allowed (Advanced
// mode) — these only seed the Default-mode select and the auto-filled default.
export const MODEL_SUGGESTIONS: Record<
  string,
  { label: string; tiers: Record<ModelTier, string>; options: string[] }
> = {
  openai: {
    label: 'OpenAI',
    tiers: { lite: 'gpt-4o-mini', flash: 'gpt-4o', pro: 'o1' },
    options: ['gpt-4o-mini', 'gpt-4o', 'o1', 'o1-mini', 'gpt-4-turbo'],
  },
  anthropic: {
    label: 'Anthropic',
    tiers: {
      lite: 'claude-haiku-4-5',
      flash: 'claude-sonnet-4-6',
      pro: 'claude-opus-4-8',
    },
    options: ['claude-haiku-4-5', 'claude-sonnet-4-6', 'claude-opus-4-8'],
  },
  'openai-compatible': {
    label: 'OpenAI-compatible (custom)',
    tiers: { lite: '', flash: '', pro: '' },
    options: [],
  },
};

export type AgentDefinition = {
  scope_id: string;
  soul_md: string;
  agents_md: string;
};

export type AgentDefinitionDraft = {
  soul_md: string;
  agents_md: string;
  warnings?: string[];
};

export type AgentSkill = {
  id: string;
  scope_id: string;
  name: string;
  description: string;
  body: string;
  enabled: boolean;
  status: string; // active | proposed
  origin: string; // user | reflect
  updated_at: string;
};

// AgentSkillInput is the writable subset for user-authored skills. The backend
// owns id/scope/origin/status; the Settings UI only edits the human-facing
// metadata, body, and whether the skill is enabled.
export type AgentSkillInput = {
  name: string;
  description: string;
  body: string;
  enabled: boolean;
};

export type AgentMemory = {
  id: string;
  scope_id: string;
  kind: string;
  content: string;
  tags: string[];
  confidence: number;
  source_run_id: string;
  created_at: string;
};

export type AgentRun = {
  id: string;
  project_id: string;
  agent_id: string;
  trigger: string;
  status: string;
  token_input: number;
  token_output: number;
  cost_usd: number;
  summary: string;
  started_at: string;
  finished_at?: string;
};

export type AgentToolCall = {
  id: string;
  run_id: string;
  tool: string;
  args_json: string;
  allowed: boolean;
  result_meta: string;
  duration_ms: number;
  created_at: string;
};

// AgentLLMCall is one model invocation inside a run — the deepest tier of the
// loop trace. Mirrors storage.AgentLLMCall; messages_json/tool_calls_json are
// JSON strings the UI parses lazily for the expanded view.
export type AgentLLMCall = {
  id: string;
  run_id: string;
  provider: string;
  model: string;
  messages_json: string;
  tools: string[];
  response: string;
  tool_calls_json: string;
  stop_reason: string;
  token_input: number;
  token_output: number;
  cost_usd: number;
  latency_ms: number;
  streamed: boolean;
  error: string;
  created_at: string;
};

// --- AgentCore Lab (test + explain modes) ---
// These mirror agentcore.LabStep / LabService output: one read model powers both
// the live (explain) and replayed (historical) per-step views, so a run reads the
// same either way. Tool args are placeholder-form ({{cred:NAME}}) — never secrets.

export type LabSkillRef = {
  id: string;
  name: string;
  description: string;
};

export type LabToolCall = {
  id: string;
  name: string;
  args: string;
  result: string;
  allowed: boolean;
  error?: string;
};

export type LabStep = {
  index: number;
  turn: number;
  kind: 'turn' | 'compaction';
  system: string;
  persona: string;
  memory: string[];
  context: { role: string; content: string }[];
  skills_advertised: LabSkillRef[];
  skills_loaded: string[];
  tools: string[];
  tool_calls: LabToolCall[];
  response: string;
  stop_reason?: string;
  error?: string;
  summary?: string;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  cum_tokens_in: number;
  cum_tokens_out: number;
  cum_cost_usd: number;
};

export type LabTestResult = {
  run_id: string;
  status: 'pass' | 'fail' | 'error' | 'blocked';
  expected: string;
  actual: string;
  // verdict reports how status was decided: 'exact' string match or 'judge'
  // (LLM rubric over the criteria); rationale is the judge's one-line reason.
  verdict?: 'exact' | 'judge';
  rationale?: string;
  diff?: string;
  steps: LabStep[];
  setup_prompt?: string;
};

// LabEvent is one SSE frame of an explain run (run | step | done | error).
export type LabEvent = {
  type: 'run' | 'step' | 'done' | 'error';
  run_id?: string;
  steps?: LabStep[];
  current?: number;
  status?: string;
  final?: string;
  error?: string;
};

export type AgentLabCase = {
  id: string;
  scope_id: string;
  name: string;
  input: string;
  expected: string;
  last_status: string;
  last_run_id?: string;
  created_at: string;
  updated_at: string;
};

export type LabExplainHandlers = {
  onRun?: (runID: string) => void;
  onStep?: (steps: LabStep[], current: number) => void;
  onDone?: (steps: LabStep[], status: string, final: string) => void;
  onError?: (message: string) => void;
};

// Agent is one first-class agent in a project (AgentGarden). The default agent's
// id equals its project_id by construction.
export type Agent = {
  id: string;
  project_id: string;
  name: string;
  slug: string;
  is_default: boolean;
  enabled: boolean;
  autonomy: string;
  created_at: string;
  updated_at: string;
};

// AgentMonitorRow is an Agent flattened with its run rollup — the unit of the
// /agents/monitor overview. Agent-agnostic: every aggregate keys on the agent id,
// so a newly added agent appears here with no per-agent code.
export type AgentMonitorRow = Agent & {
  run_count: number;
  running_count: number;
  error_count: number;
  token_input: number;
  token_output: number;
  cost_usd: number;
  last_run_at?: string;
};

export type AgentRecommendation = {
  id: string;
  project_id: string;
  run_id?: string;
  category: string;
  title: string;
  rationale: string;
  evidence_json: string;
  impact_score: number;
  status: string; // open | accepted | dismissed
  ack_note: string;
  created_at: string;
};

// AgentToolCatalogEntry mirrors agentanalyst.ToolCatalogEntry — one selectable
// tool the project can grant an agent. `configurable` tools (e.g. http_request)
// carry per-agent config (the host allowlist).
export type AgentToolCatalogEntry = {
  name: string;
  title: string;
  description: string;
  configurable: boolean;
};

// AgentToolSelection is a stored per-agent grant. `config` is a JSON string
// (the backend keeps it opaque); the tools panel parses it for the known shapes.
export type AgentToolSelection = {
  name: string;
  enabled: boolean;
  config: string;
};

export type AgentToolsResponse = {
  catalog: AgentToolCatalogEntry[];
  selections: AgentToolSelection[];
};

// AgentDelegateSelection is one stored delegation grant: this agent may hand
// tasks to that agent via spawn_subagent. Self-delegation is built in and never
// appears here.
export type AgentDelegateSelection = {
  agent_id: string;
  name: string;
  slug: string;
  enabled: boolean;
};

export type AgentDelegatesResponse = {
  agents: Agent[]; // the project's roster (candidates, includes self — filter it out)
  selections: AgentDelegateSelection[];
};

// AgentTrigger is what starts a run (AgentGarden §7): a `schedule` (cron) or a
// `webhook` (the unguessable `webhook_token` is the ingress address; an optional
// `hmac_secret_name` names a vault secret used to authenticate the body).
export type AgentTrigger = {
  id: string;
  kind: 'schedule' | 'webhook' | string;
  enabled: boolean;
  cron: string;
  webhook_token: string;
  prompt_template: string;
  hmac_secret_name: string;
  created_at: string;
  updated_at: string;
};

export type AgentTriggerInput = {
  kind: string;
  enabled: boolean;
  cron: string;
  prompt_template: string;
  hmac_secret_name: string;
};

export type AgentToolTrace = { tool: string; allowed: boolean; reason?: string; error?: string; result_meta?: string };

// AgentResultCard mirrors agentcore.ResultCard: a compact, structured answer the
// orchestrator attaches to a data reply so the UI renders a stat block or a small
// chart instead of prose alone. `kind` is 'stat' (use `stats`) or 'series' (use
// `points`).
export type AgentCardStat = { label: string; value: string };
export type AgentCardPoint = { label: string; value: number };
export type AgentResultCard = {
  title: string;
  kind: 'stat' | 'series';
  unit?: string;
  stats?: AgentCardStat[];
  points?: AgentCardPoint[];
};

// AgentChatTurn is one prior message the client replays as conversation history
// (client-held, no server-side conversation store). Only user/assistant roles.
// Legacy: the conversation store (below) derives history server-side instead.
export type AgentChatTurn = { role: 'user' | 'assistant'; content: string };

// AgentConversation is a server-side durable thread (DESIGN-CONVERSATION-STORE.md):
// the source of truth a second machine/user can load and continue. leaf_entry_id is
// the server-owned pointer to the conversation's current head.
export type AgentConversation = {
  id: string;
  project_id: string;
  agent_id: string;
  title: string;
  leaf_entry_id?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
};

// AgentConversationEntry is one immutable typed entry in a conversation's
// append-only log. `seq` is the sync cursor (clients read entries > seq). `kind`
// is message | compaction | tool_trace | step | …; for message entries `role` and
// the payload's `text` carry the turn. `author_user_id` is empty for the agent.
export type AgentConversationEntry = {
  id: string;
  conversation_id: string;
  parent_id?: string;
  seq: number;
  kind: string;
  role: string;
  // agent_id stamps which agent handled this entry (the per-message agent
  // override). Empty for entries written before the column existed and for the
  // project's default agent.
  agent_id?: string;
  author_user_id?: string;
  run_id?: string;
  turn: number;
  payload_json: string;
  token_estimate: number;
  created_at: string;
};

export type AgentChatResult = {
  run_id: string;
  final: string;
  tool_calls: AgentToolTrace[];
  usage: { input_tokens: number; output_tokens: number; cost_usd?: number };
  turns: number;
  card?: AgentResultCard | null;
  route?: string; // 'smalltalk' | 'data'
};

// AgentChatSteered is the auto-route outcome: the message was injected into a run
// already live for this conversation (a mid-run correction or follow-up), so this
// request produces no answer of its own — the answer keeps flowing on the original,
// still-open stream. `mode` is 'steer' (applied next turn) or 'followup'.
export type AgentChatSteered = { steered: true; mode: 'steer' | 'followup' };

// AgentChatStreamResult is what a streamed turn resolves to: a finished run, or a
// steered acknowledgement when the backend folded the message into a live run.
export type AgentChatStreamResult = AgentChatResult | AgentChatSteered;

// isSteered narrows a stream result to the auto-route case.
export function isSteered(r: AgentChatStreamResult): r is AgentChatSteered {
  return (r as AgentChatSteered).steered === true;
}

// Callbacks for the SSE chat stream: tokens arrive incrementally, plain-language
// progress notes while a data question runs, tool traces (debug only) as each
// call completes, and an optional result card; the resolved promise carries the
// final persisted run.
export type AgentChatStreamHandlers = {
  onToken?: (token: string) => void;
  onProgress?: (note: string) => void;
  onCard?: (card: AgentResultCard) => void;
  onTool?: (tool: AgentToolTrace) => void;
  onError?: (message: string) => void;
  // The run id, emitted before the first token, so the client can persist it and
  // reattach to the (background-continuing) run after navigating away mid-stream.
  onRunID?: (runID: string) => void;
  // A tool call is starting (name only): lets the step timeline show it as
  // in-flight before its completed `onTool` trace lands.
  onToolStart?: (tool: string) => void;
};

export const apiBase = () => process.env.NEXT_PUBLIC_AGENTRAY_API_URL || 'http://localhost:8088';

// parseSSEFrame splits one `event:`/`data:` server-sent-event frame into its
// event name and parsed JSON payload. Returns null for keep-alive/blank frames.
function parseSSEFrame(frame: string): { event: string; data: Record<string, unknown> } | null {
  let event = 'message';
  const dataLines: string[] = [];
  for (const line of frame.split('\n')) {
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }
  if (!dataLines.length) return null;
  try {
    return { event, data: JSON.parse(dataLines.join('\n')) };
  } catch {
    return null;
  }
}

// agentQuery builds the `?agent=` selector the per-agent endpoints use (Lab,
// AgentGarden definition/skills/tools/secrets/triggers); empty selects the
// project's default agent. project_id is appended later by withProject.
function agentQuery(agentID: string): string {
  return agentID ? `?agent=${encodeURIComponent(agentID)}` : '';
}

export class AgentRayAPI {
  constructor(
    private readonly projectID = '',
    private readonly apiKey = '',
  ) {}

  me() {
    return this.get<AuthState>('/api/auth/me');
  }

  signup(input: { email: string; name: string; password: string; workspace_name: string; project_name: string }) {
    return this.request<AuthState>('/api/auth/signup', {
      method: 'POST',
      body: JSON.stringify(input),
    });
  }

  login(email: string, password: string) {
    return this.request<AuthState>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    });
  }

  logout() {
    return this.request<void>('/api/auth/logout', { method: 'POST' });
  }

  updateUser(name: string) {
    return this.request<AuthState>(this.withProject('/api/users/me'), {
      method: 'PUT',
      body: JSON.stringify({ name }),
    });
  }

  workspaces() {
    return this.get<{ workspaces: Workspace[] }>('/api/workspaces');
  }

  createWorkspace(name: string) {
    return this.post<{ workspace: Workspace }>('/api/workspaces', { name });
  }

  updateWorkspace(id: string, name: string) {
    return this.request<{ workspace: Workspace }>(`/api/workspaces/${id}`, {
      method: 'PUT',
      body: JSON.stringify({ name }),
    });
  }

  workspaceProjects(workspaceID: string) {
    return this.get<{ projects: Project[] }>(`/api/workspaces/${workspaceID}/projects`);
  }

  workspaceUsage(workspaceID: string, filters: Filters) {
    return this.get<{ usage: WorkspaceUsage }>(`/api/workspaces/${workspaceID}/usage?${new URLSearchParams(filterParams(filters)).toString()}`);
  }

  workspaceMembers(workspaceID: string) {
    return this.get<{ members: WorkspaceMember[] }>(`/api/workspaces/${workspaceID}/members`);
  }

  workspaceAuditLogs(workspaceID: string, limit = 10) {
    return this.get<{ logs: WorkspaceAuditLog[] }>(`/api/workspaces/${workspaceID}/audit-logs?limit=${limit}`);
  }

  addWorkspaceMember(workspaceID: string, email: string, role: WorkspaceRole) {
    return this.post<{ member: WorkspaceMember }>(`/api/workspaces/${workspaceID}/members`, { email, role });
  }

  updateWorkspaceMemberRole(workspaceID: string, userID: string, role: WorkspaceRole) {
    return this.request<{ member: WorkspaceMember }>(`/api/workspaces/${workspaceID}/members/${userID}`, {
      method: 'PUT',
      body: JSON.stringify({ role }),
    });
  }

  removeWorkspaceMember(workspaceID: string, userID: string) {
    return this.request<void>(`/api/workspaces/${workspaceID}/members/${userID}`, { method: 'DELETE' });
  }

  createWorkspaceProject(workspaceID: string, name: string) {
    return this.post<{ project: Project }>(`/api/workspaces/${workspaceID}/projects`, { name });
  }

  project() {
    return this.get<{ project: Project }>('/api/projects');
  }

  createProject(name: string, workspaceID = '') {
    return this.request<{ project: Project }>('/api/projects', {
      method: 'POST',
      body: JSON.stringify({ name, workspace_id: workspaceID }),
    });
  }

  updateProject(projectID: string, name: string) {
    return this.request<{ project: Project }>(`/api/projects/${projectID}`, {
      method: 'PUT',
      body: JSON.stringify({ name }),
    });
  }

  rotateKey(projectID: string) {
    return this.post<{ project: Project }>(`/api/projects/${projectID}/rotate-key`, {});
  }

  activity(filters: Filters) {
    return this.get<{ project: Project; summary: ActivitySummary }>(`/api/activity?${new URLSearchParams(filterParams(filters)).toString()}`);
  }

  insight(type: string, filters: Filters, metric = '', steps: string[] = []) {
    const params = new URLSearchParams(filterParams(filters));
    params.set('type', type);
    if (metric) params.set('metric', metric);
    if (steps.length > 0) params.set('steps', steps.join(','));
    return this.get<{ project: Project; insight: InsightResult }>(`/api/insights/run?${params.toString()}`);
  }

  templates() {
    return this.get<{ templates: DashboardTemplate[] }>('/api/templates');
  }

  applyTemplate(templateID: string) {
    return this.post<{ dashboard: Dashboard; charts: Chart[] }>(`/api/templates/${templateID}/apply`, {});
  }

  cloneTemplateChart(templateID: string, chartID: string, dashboardID: string) {
    return this.post<{ chart: Chart }>(`/api/templates/${templateID}/charts/${chartID}/clone`, { dashboard_id: dashboardID });
  }

  marketplaceAgents() {
    return this.get<{ agents: AgentPreset[] }>('/api/marketplace/agents');
  }

  installAgentPreset(slug: string) {
    return this.post<{ agent: Agent }>(`/api/marketplace/agents/${slug}/install`, {});
  }

  // Agent grants: a workspace owns agents and assigns them into projects. These
  // act on this client's project (construct AgentRayAPI(targetProjectID) to
  // assign into a different product).
  workspaceAgents() {
    return this.get<{ agents: Agent[] }>('/api/agent/workspace-agents');
  }

  agentGrants(agentID: string) {
    return this.get<{ grants: AgentGrant[] }>(`/api/agent/agents/${agentID}/grants`);
  }

  grantAgent(agentID: string, scopes: AgentScopes) {
    return this.post<{ grant: AgentGrant }>(`/api/agent/agents/${agentID}/grant`, { scopes });
  }

  revokeAgent(agentID: string) {
    return this.request<void>(this.withProject(`/api/agent/agents/${agentID}/grant`), { method: 'DELETE' });
  }

  webAnalytics(filters: Filters) {
    return this.get<{ project: Project; web_analytics: WebAnalytics }>(`/api/web-analytics?${new URLSearchParams(filterParams(filters)).toString()}`);
  }

  persons(filters: Filters) {
    return this.get<{ project: Project; persons: PersonsSummary }>(`/api/persons?${new URLSearchParams(filterParams(filters)).toString()}`);
  }

  cohorts(filters: Filters, segment = 'all') {
    const params = new URLSearchParams(filterParams(filters));
    if (segment !== 'all') params.set('segment', segment);
    return this.get<{ project: Project; cohorts: CohortAnalysis }>(`/api/cohorts?${params.toString()}`);
  }

  cohortAudiences() {
    return this.get<{ project: Project; audiences: ProjectAudience[] }>('/api/cohorts/audiences');
  }

  createCohortAudience(input: AudienceInput) {
    return this.post<{ audience: ProjectAudience }>('/api/cohorts/audiences', input);
  }

  updateCohortAudience(id: string, input: AudienceInput) {
    return this.request<{ audience: ProjectAudience }>(this.withProject(`/api/cohorts/audiences/${id}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  deleteCohortAudience(id: string) {
    return this.request<void>(this.withProject(`/api/cohorts/audiences/${id}`), { method: 'DELETE' });
  }

  // Subscription mapping: the per-project config that lets cohort audiences read
  // point-in-time subscription status. GET always returns a mapping (Stripe-shaped
  // defaults with configured=false until saved); PUT validates + persists it.
  subscriptionMapping() {
    return this.get<{ project: Project; mapping: SubscriptionMapping }>('/api/subscription/mapping');
  }

  saveSubscriptionMapping(input: SubscriptionMappingInput) {
    return this.request<{ mapping: SubscriptionMapping }>(this.withProject('/api/subscription/mapping'), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  exploreEvents(filters: Filters) {
    return this.get<{ project: Project; explorer: EventExplorer }>(`/api/events/explore?${new URLSearchParams(filterParams(filters)).toString()}`);
  }

  // eventNames returns the project's distinct event-name catalog (all history,
  // most active first) to power the event-name autocomplete.
  eventNames() {
    return this.get<{ project: Project; names: EventCatalogEntry[] }>('/api/events/names');
  }

  agentReplay(sessionID: string) {
    return this.get<{ project: Project; replay: AgentReplay }>(`/api/sessions/${encodeURIComponent(sessionID)}/replay`);
  }

  savedQueries() {
    return this.get<{ project: Project; saved_queries: SavedQuery[] }>('/api/saved-queries');
  }

  createSavedQuery(naturalLanguage: string, generatedSQL: string, verified: boolean) {
    return this.post<{ saved_query: SavedQuery }>('/api/saved-queries', {
      natural_language: naturalLanguage,
      generated_sql: generatedSQL,
      verified,
    });
  }

  runSavedQuery(id: string) {
    return this.post<{ result: SavedQueryResult }>(`/api/saved-queries/${id}/run`, {});
  }

  renameSavedQuery(id: string, naturalLanguage: string) {
    return this.request<{ saved_query: SavedQuery }>(this.withProject(`/api/saved-queries/${id}`), {
      method: 'PATCH',
      body: JSON.stringify({ natural_language: naturalLanguage }),
    });
  }

  deleteSavedQuery(id: string) {
    return this.request<void>(this.withProject(`/api/saved-queries/${id}`), { method: 'DELETE' });
  }

  runSQL(sql: string) {
    return this.post<{ rows: Array<Record<string, unknown>>; generated_at: string }>('/api/sql/run', { sql });
  }

  dashboards() {
    return this.get<{ project: Project; dashboards: Dashboard[] }>('/api/dashboards');
  }

  createDashboard(name: string, description: string) {
    return this.post<{ dashboard: Dashboard }>('/api/dashboards', { name, description });
  }

  updateDashboard(id: string, name: string, description: string) {
    return this.request<{ dashboard: Dashboard }>(this.withProject(`/api/dashboards/${id}`), {
      method: 'PUT',
      body: JSON.stringify({ name, description }),
    });
  }

  deleteDashboard(id: string) {
    return this.request<void>(this.withProject(`/api/dashboards/${id}`), { method: 'DELETE' });
  }

  charts(dashboardID: string) {
    return this.get<{ project: Project; charts: Chart[] }>(`/api/dashboards/${dashboardID}/charts`);
  }

  createChart(dashboardID: string, input: ChartInput) {
    return this.post<{ chart: Chart }>(`/api/dashboards/${dashboardID}/charts`, input);
  }

  updateChart(id: string, input: ChartInput) {
    return this.request<{ chart: Chart }>(this.withProject(`/api/charts/${id}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  deleteChart(id: string) {
    return this.request<void>(this.withProject(`/api/charts/${id}`), { method: 'DELETE' });
  }

  // reorderCharts persists a new board order — chartIDs in their displayed order.
  reorderCharts(dashboardID: string, chartIDs: string[]) {
    return this.request<void>(this.withProject(`/api/dashboards/${dashboardID}/charts/order`), {
      method: 'PUT',
      body: JSON.stringify({ chart_ids: chartIDs }),
    });
  }

  // --- Alerting (#1) ---

  alertRules() {
    return this.get<{ rules: AlertRule[] }>('/api/alerts/rules');
  }

  createAlertRule(input: AlertRuleInput) {
    return this.post<{ rule: AlertRule }>('/api/alerts/rules', input);
  }

  updateAlertRule(id: string, input: AlertRuleInput) {
    return this.request<void>(this.withProject(`/api/alerts/rules/${id}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  deleteAlertRule(id: string) {
    return this.request<void>(this.withProject(`/api/alerts/rules/${id}`), { method: 'DELETE' });
  }

  alertEvents(ruleID: string) {
    return this.get<{ events: AlertEvent[] }>(`/api/alerts/rules/${ruleID}/events`);
  }

  alertChannels() {
    return this.get<{ channels: AlertChannel[] }>('/api/alerts/channels');
  }

  createAlertChannel(input: AlertChannelInput) {
    return this.post<{ channel: AlertChannel }>('/api/alerts/channels', input);
  }

  deleteAlertChannel(id: string) {
    return this.request<void>(this.withProject(`/api/alerts/channels/${id}`), { method: 'DELETE' });
  }

  // --- Per-agent budgets (#4) ---

  agentBudgets(agentID: string) {
    return this.get<{ budgets: AgentBudget[]; status: BudgetStatus }>(`/api/agent/budgets?agent=${encodeURIComponent(agentID)}`);
  }

  upsertAgentBudget(agentID: string, input: AgentBudgetInput) {
    return this.request<AgentBudget>(this.withProject(`/api/agent/budgets?agent=${encodeURIComponent(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  deleteAgentBudget(agentID: string, period: BudgetPeriod) {
    return this.request<void>(this.withProject(`/api/agent/budgets/${period}?agent=${encodeURIComponent(agentID)}`), { method: 'DELETE' });
  }

  // --- AI Agent (§8, §10) ---

  agentConfig() {
    return this.get<{ config: AgentConfig }>('/api/agent/config');
  }

  updateAgentConfig(input: AgentConfigInput) {
    return this.request<{ config: AgentConfig }>(this.withProject('/api/agent/config'), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  // Workspace model pool: the 3 tiers shared by every project/agent in the
  // workspace, configured once (owner/admin to mutate). Resolved from the active
  // project's workspace server-side.
  workspaceModels() {
    return this.get<{ config: WorkspaceModelTiers }>('/api/workspace/models');
  }

  updateWorkspaceModels(input: WorkspaceModelTiersInput) {
    return this.request<{ config: WorkspaceModelTiers }>(this.withProject('/api/workspace/models'), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  testWorkspaceModels() {
    return this.post<AgentConfigTestResult>('/api/workspace/models/test', {});
  }

  // Per-agent capabilities: which backend usecase/analytics tool groups this
  // agent can use. `agentID` selects the agent (empty = the project's default).
  agentCapabilities(agentID = '') {
    return this.get<{ capabilities: AgentCapabilityConfig }>(`/api/agent/capabilities${agentQuery(agentID)}`);
  }

  updateAgentCapabilities(scopes: AgentScopes, agentID = '') {
    return this.request<{ capabilities: AgentCapabilityConfig }>(this.withProject(`/api/agent/capabilities${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ scopes }),
    });
  }

  // Per-agent task→tier map: which workspace tier each task kind (triage/run/
  // compaction/reflection) draws from. `agentID` selects the agent (empty = the
  // project's default agent).
  agentTaskTiers(agentID = '') {
    return this.get<{ tiers: AgentTaskTiers }>(`/api/agent/task-tiers${agentQuery(agentID)}`);
  }

  updateAgentTaskTiers(tiers: AgentTaskTiers, agentID = '') {
    return this.request<{ tiers: AgentTaskTiers }>(this.withProject(`/api/agent/task-tiers${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ tiers }),
    });
  }

  // Definition + skills are per-agent: `agentID` selects the agent (empty = the
  // project's default agent). The `?agent=` selector is added before withProject
  // appends project_id.
  agentDefinition(agentID = '') {
    return this.get<{ definition: AgentDefinition }>(`/api/agent/definition${agentQuery(agentID)}`);
  }

  updateAgentDefinition(soul_md: string, agents_md: string, agentID = '') {
    return this.request<{ definition: AgentDefinition }>(this.withProject(`/api/agent/definition${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ soul_md, agents_md }),
    });
  }

  generateAgentDefinition(prompt: string, agentID = '') {
    return this.request<{ definition: AgentDefinitionDraft }>(this.withProject(`/api/agent/definition/generate${agentQuery(agentID)}`), {
      method: 'POST',
      body: JSON.stringify({ prompt }),
    });
  }

  agentSkills(agentID = '') {
    return this.get<{ skills: AgentSkill[] }>(`/api/agent/skills${agentQuery(agentID)}`);
  }

  createAgentSkill(input: AgentSkillInput, agentID = '') {
    return this.post<{ skill: AgentSkill }>(`/api/agent/skills${agentQuery(agentID)}`, input);
  }

  updateAgentSkill(id: string, input: AgentSkillInput, agentID = '') {
    return this.request<{ skill: AgentSkill }>(this.withProject(`/api/agent/skills/${id}${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  approveAgentSkill(id: string, agentID = '') {
    return this.post<{ ok: boolean }>(`/api/agent/skills/${id}/approve${agentQuery(agentID)}`, {});
  }

  deleteAgentSkill(id: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/skills/${id}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  agentMemory() {
    return this.get<{ memory: AgentMemory[] }>('/api/agent/memory');
  }

  deleteAgentMemory(id: string) {
    return this.request<void>(this.withProject(`/api/agent/memory/${id}`), { method: 'DELETE' });
  }

  agentRuns(limit = 50) {
    return this.get<{ runs: AgentRun[] }>(`/api/agent/runs?limit=${limit}`);
  }

  agentRun(runID: string) {
    return this.get<{ run: AgentRun; tool_calls: AgentToolCall[]; llm_calls: AgentLLMCall[] }>(`/api/agent/runs/${runID}`);
  }

  // sessionRun reattaches a returning client to the latest run of a conversation
  // (the one it streamed before navigating away). Rejects (404) when the session
  // has no run yet. A done run's `summary` is the final answer; `tool_calls` is the
  // persisted tool trace, used to rebuild the step timeline lost on reload.
  sessionRun(sessionID: string) {
    return this.get<{ run: AgentRun; tool_calls: AgentToolCall[] }>(`/api/agent/sessions/${encodeURIComponent(sessionID)}/run`);
  }

  // --- agent monitoring console (/agents/monitor) ---

  agentMonitor() {
    return this.get<{ agents: AgentMonitorRow[] }>('/api/agents/monitor');
  }

  agentMonitorDetail(agentID: string, limit = 50) {
    return this.get<{ agent: AgentMonitorRow; runs: AgentRun[] }>(`/api/agents/${agentID}/monitor?limit=${limit}`);
  }

  agentRecommendations() {
    return this.get<{ recommendations: AgentRecommendation[] }>('/api/agent/recommendations');
  }

  ackRecommendation(id: string, status: 'accepted' | 'dismissed', note = '') {
    return this.post<{ ok: boolean }>(`/api/agent/recommendations/${id}/ack`, { status, note });
  }

  // agentChat sends a non-streamed turn. sessionID is the conversation id used by
  // the auto-route: when a run is already live for it the backend folds this
  // message into that run and returns a steered acknowledgement instead of a run.
  // `agentID` targets a specific agent's persona for the turn (empty = the
  // project's default agent, which the orchestrator auto-routes).
  agentChat(message: string, history: AgentChatTurn[] = [], sessionID = '', agentID = '') {
    return this.post<AgentChatStreamResult>(`/api/agent/chat${agentQuery(agentID)}`, { message, history, session_id: sessionID });
  }

  // agentChatStream opens the SSE variant of the chat endpoint, invoking the
  // handlers as tokens/progress/cards/tool-traces arrive and resolving with the
  // final run. `history` replays prior turns (client-held). `sessionID` keys the
  // auto-route: when a run is already live for it, the backend injects this message
  // into that run and replies with a single `steered` frame (no answer here — it
  // continues on the original stream); `mode` picks steer (default) or followup.
  // `agentID` targets a specific agent's persona (empty = the project's default,
  // auto-routed by the orchestrator).
  // Falls back to throwing on a non-stream error response so callers can surface it.
  async agentChatStream(
    message: string,
    handlers: AgentChatStreamHandlers = {},
    history: AgentChatTurn[] = [],
    opts: { sessionID?: string; mode?: 'steer' | 'followup'; agentID?: string; signal?: AbortSignal } = {},
  ): Promise<AgentChatStreamResult> {
    const response = await fetch(`${apiBase()}${this.withProject(`/api/agent/chat${agentQuery(opts.agentID ?? '')}`)}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify({ message, history, session_id: opts.sessionID ?? '', mode: opts.mode ?? 'steer' }),
      // Only an explicit Stop aborts the stream. Navigating away must NOT pass a
      // signal here — the run keeps going server-side and is reattached on return.
      signal: opts.signal,
    });
    return this.consumeChatSSE(response, handlers);
  }

  // consumeChatSSE reads the shared chat SSE contract (token/run/progress/card/
  // tool/tool_start/error/done/steered) off a streamed response and resolves with
  // the final run (or a steered ack). Shared by agentChatStream (legacy /chat) and
  // conversationSend (the conversation store's /messages), which speak the same
  // event shape — only the endpoint and history source differ.
  private async consumeChatSSE(
    response: Response,
    handlers: AgentChatStreamHandlers,
  ): Promise<AgentChatStreamResult> {
    if (!response.ok || !response.body) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload.error || payload.message || `AgentRay API returned ${response.status}`);
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let result: AgentChatStreamResult | null = null;

    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let sep = buffer.indexOf('\n\n');
      while (sep >= 0) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const evt = parseSSEFrame(frame);
        if (evt) {
          if (evt.event === 'token') handlers.onToken?.(String(evt.data.token ?? ''));
          else if (evt.event === 'run') handlers.onRunID?.(String(evt.data.run_id ?? ''));
          else if (evt.event === 'progress') handlers.onProgress?.(String(evt.data.note ?? ''));
          else if (evt.event === 'card') handlers.onCard?.(evt.data as unknown as AgentResultCard);
          else if (evt.event === 'tool') handlers.onTool?.(evt.data as unknown as AgentToolTrace);
          else if (evt.event === 'tool_start') handlers.onToolStart?.(String(evt.data.tool ?? ''));
          else if (evt.event === 'error') handlers.onError?.(String(evt.data.error ?? 'stream error'));
          else if (evt.event === 'done') result = evt.data as unknown as AgentChatResult;
          // The auto-route folded this message into a live run; acknowledge and stop.
          else if (evt.event === 'steered') {
            result = { steered: true, mode: (String(evt.data.mode ?? 'steer') as 'steer' | 'followup') };
          }
        }
        sep = buffer.indexOf('\n\n');
      }
    }
    if (!result) throw new Error('chat stream ended without a result');
    return result;
  }

  // --- conversation store (DESIGN-CONVERSATION-STORE.md): server-side durable
  // threads. Replaces client-held history — a second machine/user loads the thread
  // from the server and continues it, and the model context is derived server-side.

  // createConversation opens a server-side thread for an agent and returns its row.
  // Navigate by `conversation.id` instead of a locally-minted session id.
  async createConversation(agentID = '', title = '') {
    const r = await this.post<{ conversation: AgentConversation }>('/api/agent/conversations', {
      agent_id: agentID,
      title,
    });
    return r.conversation;
  }

  // listConversations returns the project's recent threads, newest activity first.
  listConversations() {
    return this.get<{ conversations: AgentConversation[] }>('/api/agent/conversations');
  }

  // getConversation loads a thread plus its entries since a sync cursor (`since`,
  // default 0 = the whole log) and the current leaf seq. This is the multi-machine
  // load/resume path: render the human view from entries, then poll/stream from
  // leaf_seq onward.
  getConversation(id: string, since = 0) {
    const q = since > 0 ? `?since=${since}` : '';
    return this.get<{ conversation: AgentConversation; entries: AgentConversationEntry[]; leaf_seq: number }>(
      `/api/agent/conversations/${encodeURIComponent(id)}${q}`,
    );
  }

  // conversationSend posts a user message to a conversation and streams the agent's
  // turn over the shared chat SSE contract. The user turn is appended and the model
  // History is derived server-side — the client sends NO history. Same handler shape
  // as agentChatStream; `mode` picks steer/followup when a run is already live on the
  // thread. Only an explicit Stop should pass a signal (navigating away keeps the run
  // alive server-side, to be reloaded via getConversation).
  async conversationSend(
    id: string,
    message: string,
    handlers: AgentChatStreamHandlers = {},
    opts: { mode?: 'steer' | 'followup'; signal?: AbortSignal; agentID?: string } = {},
  ): Promise<AgentChatStreamResult> {
    // agent_id, when set, switches the acting agent from this message onward (the
    // per-message override). The backend stamps the new entries and persists the
    // switch, so subsequent messages continue with the chosen agent; past entries
    // keep the agent that handled them.
    const body: Record<string, unknown> = { message, mode: opts.mode ?? 'steer' };
    if (opts.agentID) body.agent_id = opts.agentID;
    const response = await fetch(`${apiBase()}${this.withProject(`/api/agent/conversations/${encodeURIComponent(id)}/messages`)}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(body),
      signal: opts.signal,
    });
    return this.consumeChatSSE(response, handlers);
  }

  triggerAgentRun() {
    return this.post<{ queued: boolean }>('/api/agent/run', {});
  }

  // --- AgentGarden agents (§3): first-class agent identity per project ---

  agents() {
    return this.get<{ agents: Agent[] }>('/api/agent/agents');
  }

  createAgent(name: string, slug = '') {
    return this.post<Agent>('/api/agent/agents', { name, slug });
  }

  updateAgent(id: string, name: string, enabled: boolean) {
    return this.request<Agent>(this.withProject(`/api/agent/agents/${id}`), {
      method: 'PUT',
      body: JSON.stringify({ name, enabled }),
    });
  }

  deleteAgent(id: string) {
    return this.request<void>(this.withProject(`/api/agent/agents/${id}`), { method: 'DELETE' });
  }

  // --- AgentGarden self-service config (§6 tools, §5 secrets, §7 triggers) ---
  // `agentID` selects the agent (empty = the project's default agent). Secrets
  // are names-only on read — values are write-only.

  agentSecrets(agentID = '') {
    return this.get<{ names: string[] }>(`/api/agent/secrets${agentQuery(agentID)}`);
  }

  setAgentSecret(name: string, value: string, agentID = '') {
    return this.request<{ ok: boolean }>(this.withProject(`/api/agent/secrets/${encodeURIComponent(name)}${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ value }),
    });
  }

  deleteAgentSecret(name: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/secrets/${encodeURIComponent(name)}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  agentTools(agentID = '') {
    return this.get<AgentToolsResponse>(`/api/agent/tools${agentQuery(agentID)}`);
  }

  // setAgentTool grants/updates a tool. `config` is sent as a raw object (the
  // backend stores it as JSON) — not a string — so http_request's allow_hosts
  // round-trips correctly.
  setAgentTool(name: string, enabled: boolean, config: Record<string, unknown>, agentID = '') {
    return this.request<{ ok: boolean }>(this.withProject(`/api/agent/tools/${encodeURIComponent(name)}${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ enabled, config }),
    });
  }

  deleteAgentTool(name: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/tools/${encodeURIComponent(name)}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  agentDelegates(agentID = '') {
    return this.get<AgentDelegatesResponse>(`/api/agent/delegates${agentQuery(agentID)}`);
  }

  setAgentDelegate(delegateAgentID: string, enabled: boolean, agentID = '') {
    return this.request<{ ok: boolean }>(this.withProject(`/api/agent/delegates/${encodeURIComponent(delegateAgentID)}${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify({ enabled }),
    });
  }

  deleteAgentDelegate(delegateAgentID: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/delegates/${encodeURIComponent(delegateAgentID)}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  agentTriggers(agentID = '') {
    return this.get<{ triggers: AgentTrigger[] }>(`/api/agent/triggers${agentQuery(agentID)}`);
  }

  createAgentTrigger(input: AgentTriggerInput, agentID = '') {
    return this.post<AgentTrigger>(`/api/agent/triggers${agentQuery(agentID)}`, input);
  }

  updateAgentTrigger(id: string, input: Omit<AgentTriggerInput, 'kind'>, agentID = '') {
    return this.request<AgentTrigger>(this.withProject(`/api/agent/triggers/${id}${agentQuery(agentID)}`), {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  }

  deleteAgentTrigger(id: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/triggers/${id}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  // --- AgentCore Lab (/agents/:id/lab) ---
  // agentID selects the agent under test; empty targets the project's default
  // agent. Saved cases, a test-mode run (verdict vs expected), step replay for any
  // run, and the explain-mode SSE stream with advance/stop controls all live here.

  labCases(agentID = '') {
    return this.get<{ cases: AgentLabCase[] }>(`/api/agent/lab/cases${agentQuery(agentID)}`);
  }

  saveLabCase(input: { name: string; input: string; expected: string }, agentID = '') {
    return this.post<{ case: AgentLabCase }>(`/api/agent/lab/cases${agentQuery(agentID)}`, input);
  }

  deleteLabCase(id: string, agentID = '') {
    return this.request<void>(this.withProject(`/api/agent/lab/cases/${id}${agentQuery(agentID)}`), { method: 'DELETE' });
  }

  setLabCaseVerdict(id: string, status: string, runID = '', agentID = '') {
    return this.post<{ ok: boolean }>(`/api/agent/lab/cases/${id}/verdict${agentQuery(agentID)}`, { status, run_id: runID });
  }

  runLabTest(input: string, expected: string, agentID = '', caseID = '') {
    return this.post<{ result: LabTestResult }>(`/api/agent/lab/test${agentQuery(agentID)}`, { input, expected, case_id: caseID });
  }

  labReplaySteps(runID: string, agentID = '') {
    return this.get<{ steps: LabStep[] }>(`/api/agent/lab/runs/${runID}/steps${agentQuery(agentID)}`);
  }

  labAdvance(runID: string, agentID = '') {
    return this.post<{ ok: boolean }>(`/api/agent/lab/explain/${runID}/advance${agentQuery(agentID)}`, {});
  }

  labStop(runID: string, agentID = '') {
    return this.post<{ ok: boolean }>(`/api/agent/lab/explain/${runID}/stop${agentQuery(agentID)}`, {});
  }

  // labSteer injects a mid-run message into a live explain run; the run folds it
  // in at the top of its next turn. `ok:false` means no live run matched.
  labSteer(runID: string, message: string, agentID = '') {
    return this.post<{ ok: boolean }>(`/api/agent/lab/explain/${runID}/steer${agentQuery(agentID)}`, { message });
  }

  // labExplainStream opens the SSE explain run, invoking handlers as the run id,
  // each paused step view, and the terminal done frame arrive. The run pauses
  // before each step server-side; the caller drives it with labAdvance/labStop.
  async labExplainStream(input: string, handlers: LabExplainHandlers = {}, agentID = ''): Promise<void> {
    const response = await fetch(`${apiBase()}${this.withProject(`/api/agent/lab/explain${agentQuery(agentID)}`)}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify({ input }),
    });
    if (!response.ok || !response.body) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload.error || payload.message || `AgentRay API returned ${response.status}`);
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let sep = buffer.indexOf('\n\n');
      while (sep >= 0) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const evt = parseSSEFrame(frame);
        if (evt) {
          const data = evt.data as unknown as LabEvent;
          if (evt.event === 'run') handlers.onRun?.(String(data.run_id ?? ''));
          else if (evt.event === 'step') handlers.onStep?.(data.steps ?? [], Number(data.current ?? 0));
          else if (evt.event === 'done') handlers.onDone?.(data.steps ?? [], String(data.status ?? ''), String(data.final ?? ''));
          else if (evt.event === 'error') handlers.onError?.(String(data.error ?? 'stream error'));
        }
        sep = buffer.indexOf('\n\n');
      }
    }
  }

  private get<T>(path: string) {
    return this.request<T>(this.withProject(path));
  }

  private post<T>(path: string, body: unknown) {
    return this.request<T>(this.withProject(path), {
      method: 'POST',
      body: JSON.stringify(body),
    });
  }

  private withProject(path: string) {
    if (!this.projectID && !this.apiKey) {
      return path;
    }
    const separator = path.includes('?') ? '&' : '?';
    const params = new URLSearchParams();
    if (this.projectID) params.set('project_id', this.projectID);
    if (this.apiKey) params.set('api_key', this.apiKey);
    return `${path}${separator}${params.toString()}`;
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(`${apiBase()}${path}`, {
      ...init,
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        ...(init.headers || {}),
      },
    });
    if (response.status === 204) {
      return undefined as T;
    }
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(payload.message || payload.error || `AgentRay API returned ${response.status}`);
    }
    return payload as T;
  }
}

export const defaultFilters: Filters = {
  hours: 24,
  from: '',
  to: '',
  event_type: '',
  event_name: '',
  distinct_id: '',
  session_id: '',
  agent_id: '',
  model_name: '',
  search: '',
  error_only: false,
  limit: 100,
};

function filterParams(filters: Filters): Record<string, string> {
  const params: Record<string, string> = {
    hours: String(filters.hours),
    limit: String(filters.limit),
  };
  if (filters.from) params.from = new Date(filters.from).toISOString();
  if (filters.to) params.to = new Date(filters.to).toISOString();
  for (const key of ['event_type', 'event_name', 'distinct_id', 'session_id', 'agent_id', 'model_name', 'search'] as const) {
    if (filters[key]) params[key] = filters[key];
  }
  if (filters.error_only) params.error_only = 'true';
  return params;
}
