import type { Project } from '@/lib/api';

// Same prefix as the chat cache (`agentray.chat.v2.<projectID>`). Per-browser,
// not per-user: a stale id from another account is ignored, never deleted.
export const PROJECT_PREFERENCE_KEY = 'agentray.projectId';

export function readPreferredProjectID(): string {
  if (typeof window === 'undefined') return '';
  try {
    return window.localStorage.getItem(PROJECT_PREFERENCE_KEY) ?? '';
  } catch {
    return '';
  }
}

export function writePreferredProjectID(id: string) {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(PROJECT_PREFERENCE_KEY, id);
  } catch { /* private mode / quota — ignore */ }
}

export function pickPreferredProject(
  projects: Project[],
  preferredID: string,
  fallback: Project | null | undefined,
): Project | null {
  if (preferredID) {
    const match = projects.find((p) => p.id === preferredID);
    if (match) return match;
  }
  if (fallback && projects.some((p) => p.id === fallback.id)) return fallback;
  return projects[0] ?? null;
}
