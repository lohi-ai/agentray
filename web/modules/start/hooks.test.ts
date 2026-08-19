import { describe, expect, it } from 'vitest';
import type { Agent } from '@/lib/api';
import { agentForPack, installedPacks } from './hooks';

function agent(slug: string, id = slug): Agent {
  return {
    id,
    project_id: 'p1',
    name: slug,
    slug,
    is_default: false,
    enabled: true,
    autonomy: 'suggest',
    workspace_path: '',
    preset_slug: '',
    created_at: '',
    updated_at: '',
  };
}

describe('agentForPack', () => {
  it('finds the agent a pack installed as', () => {
    expect(agentForPack([agent('ops-watch')], 'ops-watch')?.slug).toBe('ops-watch');
  });

  // freeAgentSlug (store/marketplace.go) numbers a second install, so a pack
  // hired twice must still read as hired — otherwise /start tells an owner to
  // hire a teammate they already have.
  it('matches the numbered slug a second install gets', () => {
    expect(agentForPack([agent('ops-watch-2')], 'ops-watch')?.slug).toBe('ops-watch-2');
  });

  it('does not match a different pack that shares a prefix word', () => {
    expect(agentForPack([agent('marketing-lead')], 'marketing')).toBeNull();
    expect(agentForPack([agent('growth-lead')], 'growth-leader')).toBeNull();
  });

  it('misses cleanly on an empty roster or an empty slug', () => {
    expect(agentForPack([], 'ops-watch')).toBeNull();
    expect(agentForPack([agent('ops-watch')], '')).toBeNull();
  });
});

describe('installedPacks', () => {
  it('keeps only the packs the roster actually carries, in the job’s order', () => {
    const roster = [agent('growth-lead'), agent('data-analyst-3'), agent('custom')];
    expect(installedPacks(roster, ['growth-lead', 'marketing-lead', 'data-analyst'])).toEqual([
      'growth-lead',
      'data-analyst',
    ]);
  });

  it('is empty for a fresh workspace', () => {
    expect(installedPacks([], ['product-scout'])).toEqual([]);
  });
});
