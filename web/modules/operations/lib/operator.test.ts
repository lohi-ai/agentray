import { describe, expect, it } from 'vitest';
import type { Operator } from '@/lib/api';
import {
  isEditableHere,
  isTeamRun,
  lastOutcome,
  operatorStatus,
  operatorTitle,
  rank,
  runnerLabel,
  startsOn,
  webhookPath,
} from './operator';

function op(over: Partial<Operator> = {}): Operator {
  return {
    id: 't1',
    source: 'trigger',
    name: '',
    kind: 'schedule',
    enabled: true,
    cron: '0 9 * * 1',
    webhook_token: '',
    prompt_template: '',
    hmac_secret_name: '',
    agent_id: 'a1',
    agent_name: 'Ops Watch',
    agent_enabled: true,
    team_id: '',
    team_name: '',
    run_count: 0,
    running_count: 0,
    runs_24h: 0,
    errors_24h: 0,
    cost_24h: 0,
    last_run_at: '',
    last_status: '',
    last_summary: '',
    consecutive_failures: 0,
    shared_history: false,
    created_at: '',
    updated_at: '',
    ...over,
  };
}

describe('operatorStatus', () => {
  it('calls an armed, healthy trigger Armed', () => {
    expect(operatorStatus(op())).toEqual({ status: 'healthy', label: 'Armed' });
  });

  it('reports a trigger on a paused teammate as paused, not armed', () => {
    // This is the state the product most easily lies about: the trigger fires on
    // schedule and nothing runs. "Armed" would tell the owner work is happening
    // when none is, which is worse than showing no status at all.
    expect(operatorStatus(op({ agent_enabled: false }))).toEqual({ status: 'paused', label: 'Teammate paused' });
  });

  it('lets the owner’s own pause win over every other signal', () => {
    expect(operatorStatus(op({ enabled: false, consecutive_failures: 3, running_count: 1 }))).toEqual({
      status: 'paused',
      label: 'Paused',
    });
  });

  it('puts a failure streak ahead of an in-flight run', () => {
    // A run started after three failures is not evidence the operator recovered.
    expect(operatorStatus(op({ consecutive_failures: 3, running_count: 1 })).status).toBe('attention');
  });

  it('ranks failures, then active work, then armed, then paused', () => {
    const rows = [op({ enabled: false }), op(), op({ running_count: 1 }), op({ consecutive_failures: 1 })];
    expect([...rows].sort((a, b) => rank(a) - rank(b)).map((r) => operatorStatus(r).status)).toEqual([
      'attention',
      'working',
      'healthy',
      'paused',
    ]);
  });
});

describe('operatorTitle', () => {
  it('prefers the name the owner gave it', () => {
    expect(operatorTitle(op({ name: '  Monday triage  ' }))).toBe('Monday triage');
  });

  it('never titles a row with a cron expression', () => {
    // `0 9 * * 1` as a row title is unreadable; the teammate's name is at least
    // something the owner recognises.
    expect(operatorTitle(op())).toBe('Ops Watch schedule');
    expect(operatorTitle(op({ kind: 'webhook' }))).toBe('Ops Watch webhook');
  });

  it('credits the team, not the lead, when a team answers the trigger', () => {
    const row = op({ team_name: 'Growth pod' });
    expect(runnerLabel(row)).toBe('Growth pod');
    expect(isTeamRun(row)).toBe(true);
    expect(operatorTitle(row)).toBe('Growth pod schedule');
  });
});

describe('startsOn', () => {
  it('states a schedule in words and a webhook as a short path', () => {
    expect(startsOn(op())).toBe('Mondays 09:00');
    expect(startsOn(op({ kind: 'webhook', webhook_token: 'tok-wxyz' }))).toBe('POST /hook/…wxyz');
  });

  it('never yields a truncated URL someone could paste', () => {
    // A copy-pasteable-looking half address is an hour of debugging. The cell is
    // deliberately not a URL; the full one lives on the detail page behind Copy.
    expect(webhookPath('tok-wxyz')).not.toContain('http');
    expect(webhookPath('')).toBe('No address yet');
  });

  it('says so when a schedule row has no schedule', () => {
    expect(startsOn(op({ cron: '' }))).toBe('No schedule set');
  });
});

describe('lastOutcome', () => {
  it('distinguishes never-run from a clean run', () => {
    expect(lastOutcome(op())).toEqual({ text: 'Never run', failed: false });
    expect(lastOutcome(op({ last_status: 'done', last_summary: '' }))).toEqual({ text: 'Completed', failed: false });
  });

  it('counts the streak so a repeat failure does not read like a one-off', () => {
    expect(lastOutcome(op({ last_status: 'error', consecutive_failures: 4, last_summary: 'model timeout' }))).toEqual({
      text: 'Failed 4 runs in a row — model timeout',
      failed: true,
    });
    expect(lastOutcome(op({ last_status: 'error', consecutive_failures: 1 })).text).toBe('Failed');
  });
});

describe('isEditableHere', () => {
  it('refuses to edit the legacy project schedule from this surface', () => {
    // Its cron and its on/off switch are the project's autonomy setting; writing
    // them from here would change how every unattended run in the project is
    // gated, from a page that only claims to be editing one row.
    expect(isEditableHere(op({ source: 'config' }))).toBe(false);
    expect(isEditableHere(op())).toBe(true);
  });
});
