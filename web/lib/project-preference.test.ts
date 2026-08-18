import { describe, expect, it } from 'vitest';
import { pickPreferredProject } from './project-preference';
import type { Project } from '@/lib/api';

const p = (id: string): Project => ({
  id,
  name: id,
  api_key: 'k',
  created_at: '2026-01-01T00:00:00Z',
  workspace_id: 'ws',
});

describe('pickPreferredProject', () => {
  const list = [p('a'), p('b'), p('c')];

  it('prefers the stored id when it is in the list', () => {
    expect(pickPreferredProject(list, 'b', list[0])?.id).toBe('b');
  });

  it('falls back to /me project when the stored id is unknown', () => {
    expect(pickPreferredProject(list, 'gone', list[2])?.id).toBe('c');
  });

  it('falls back to the first project when neither stored nor fallback match', () => {
    expect(pickPreferredProject(list, 'gone', p('other'))?.id).toBe('a');
  });

  it('returns null when the list is empty', () => {
    expect(pickPreferredProject([], 'a', p('a'))).toBeNull();
  });
});
