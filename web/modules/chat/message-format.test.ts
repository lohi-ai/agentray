import { describe, expect, it } from 'vitest';
import type { AgentSkill } from '@/lib/api';
import { composeMessage, parseRichMessage } from './message-format';

const COMMANDS = ['goal', 'plan', 'compact', 'clear', 'agents', 'help'];
const skill = (name: string): AgentSkill =>
  ({ id: name, name, description: '', enabled: true, status: 'active' }) as AgentSkill;
const SKILLS = [skill('Cohort Analysis')];

describe('composeMessage', () => {
  // The one that matters. The server reads a directive only at the head of the
  // message, so a skill directive hoisted above it turns `/goal …` into prose and
  // the run quietly isn't gated — a failure with no error and no symptom until
  // the agent stops early.
  it('keeps a command line first, above any skill directive', () => {
    const out = composeMessage('/goal all tests pass\n/cohort-analysis check it', [], SKILLS, COMMANDS);
    expect(out.split('\n')[0]).toBe('/goal all tests pass');
    expect(out).toContain('Use your "Cohort Analysis" skill.');
  });

  // The command's argument runs to the end of its line — the same rule the server
  // parses by. A task typed on that line becomes part of the condition, which is
  // why the menu inserts `/goal ` and the placeholder asks for a condition.
  it('takes the whole first line as the argument', () => {
    const out = composeMessage('/goal all tests pass check it', [], SKILLS, COMMANDS);
    expect(out).toBe('/goal all tests pass check it');
  });

  // One rule everywhere: a /slug naming a real skill is a skill, whichever line
  // it landed on. A chip that works in the body and not in a command argument is
  // a chip the user discovers is broken only after the turn has run.
  it('resolves a skill token inside a command argument too', () => {
    const out = composeMessage('/goal the /cohort-analysis page ships', [], SKILLS, COMMANDS);
    expect(out).toBe('/goal the page ships\n\nUse your "Cohort Analysis" skill.');
  });

  // A slug that names nothing stays where it was typed — including inside an
  // argument, where it is far more likely to be a path than a mistyped skill.
  it('leaves an unknown slug in a command argument alone', () => {
    const out = composeMessage('/goal the /pricing page ships', [], SKILLS, COMMANDS);
    expect(out).toBe('/goal the /pricing page ships');
  });

  it('is unchanged for a message with no command', () => {
    const out = composeMessage('/cohort-analysis how did last week go?', [], SKILLS, COMMANDS);
    expect(out).toBe('Use your "Cohort Analysis" skill.\n\nhow did last week go?');
  });

  // An unknown slash word is prose. Swallowing it would silently drop text the
  // user typed.
  it('treats an unknown slash word as prose', () => {
    expect(composeMessage('/goals for the quarter', [], SKILLS, COMMANDS)).toBe('/goals for the quarter');
  });
});

describe('parseRichMessage', () => {
  it('round-trips a command with a skill directive under it', () => {
    const composed = composeMessage('/goal ship it\n/cohort-analysis do the work', [], SKILLS, COMMANDS);
    const parsed = parseRichMessage(composed, COMMANDS);
    expect(parsed.command).toBe('goal');
    expect(parsed.commandArg).toBe('ship it');
    expect(parsed.skills).toEqual(['Cohort Analysis']);
    expect(parsed.text).toBe('do the work');
  });

  it('reads a bare command with no argument', () => {
    const parsed = parseRichMessage('/compact', COMMANDS);
    expect(parsed.command).toBe('compact');
    expect(parsed.commandArg).toBe('');
    expect(parsed.text).toBe('');
  });

  // Before the catalog loads there is no vocabulary to match against; a command
  // line reads perfectly well as text, which is the right fallback.
  it('leaves the line as prose when no catalog is supplied', () => {
    const parsed = parseRichMessage('/compact');
    expect(parsed.command).toBeUndefined();
    expect(parsed.text).toBe('/compact');
  });
});
