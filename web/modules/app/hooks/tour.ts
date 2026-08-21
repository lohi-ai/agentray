'use client';

import { useEffect, useRef } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type Project } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import {
  projectAccess,
  tourSteps,
  nextTourStep,
  tourProgress,
  showTour,
  TOUR_SCHEDULE_CRON,
  TOUR_SCHEDULE_NAME,
  TOUR_SCHEDULE_PROMPT,
  type ProjectAccess,
  type TourInput,
  type TourStep,
} from '@/lib/ia';
import { readPreferredProjectID, writePreferredProjectID } from '@/lib/project-preference';

// useProjectAccess answers "may this person change what they are looking at?"
// from the two facts the API stamps on every project — the caller's role in the
// owning workspace and whether that workspace is the shared demo. Every write
// affordance reads it, so a viewer sees a disabled control with a reason rather
// than a live button that ends in a 403.
export function useProjectAccess(): ProjectAccess {
  const project = useAuthStore((s) => s.project);
  return projectAccess(project);
}

// useDemoWorkspace resolves the shared demo once: the workspace the API marked
// `is_demo`, and the project inside it to open. Returns nulls on an instance
// that has no demo, which is the default and every `docker compose up`.
function useDemoWorkspace() {
  const workspaces = useAuthStore((s) => s.workspaces);
  const projectID = useAuthStore((s) => s.project?.id);
  const demoWorkspace = workspaces.find((w) => w.is_demo) ?? null;

  const query = useQuery({
    queryKey: ['demo-projects', demoWorkspace?.id],
    queryFn: () => new AgentRayAPI(projectID || '').workspaceProjects(demoWorkspace!.id),
    enabled: !!demoWorkspace?.id,
    staleTime: 10 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  return {
    workspace: demoWorkspace,
    project: query.data?.projects?.[0] ?? null,
    // Undecided until the membership list has loaded at all — `workspaces` is
    // empty on the very first paint, and treating that as "no demo" would open
    // the tour on step 4 for one frame and then rewrite it.
    resolved: workspaces.length > 0 && (!demoWorkspace || !query.isPending),
  };
}

// useOwnProjects lists the projects in the workspaces the user actually owns.
// `auth.projects` cannot answer this: it holds one workspace at a time, and
// while somebody is reading the demo that one workspace is the demo's.
function useOwnProjects() {
  const workspaces = useAuthStore((s) => s.workspaces);
  const projects = useAuthStore((s) => s.projects);
  const projectID = useAuthStore((s) => s.project?.id);
  const own = workspaces.find((w) => !w.is_demo) ?? null;
  // The list we already hold, when it happens to be the right workspace's.
  const cached = own && projects.length > 0 && projects[0]?.workspace_id === own.id ? projects : null;

  const query = useQuery({
    queryKey: ['own-projects', own?.id],
    queryFn: () => new AgentRayAPI(projectID || '').workspaceProjects(own!.id),
    enabled: !!own?.id && !cached,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const list: Project[] = cached ?? query.data?.projects ?? [];
  return {
    workspace: own,
    projects: list,
    resolved: workspaces.length > 0 && (!own || !!cached || !query.isPending),
  };
}

// useTour is the whole first-run engine. Every tick it reports is a server
// answer about the user's OWN workspace — a project exists, events are
// arriving, something is armed — read for that project specifically rather than
// for whichever project the app happens to be pointed at, so the progress is
// the same on their phone as it is here and the same after a reload.
export function useTour(agentName?: string) {
  const queryClient = useQueryClient();
  const project = useAuthStore((s) => s.project);
  const setProject = useAuthStore((s) => s.setProject);
  const setProjects = useAuthStore((s) => s.setProjects);
  const setSelectedWorkspaceID = useAuthStore((s) => s.setSelectedWorkspaceID);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);

  const demo = useDemoWorkspace();
  const own = useOwnProjects();
  const ownProject = own.projects[0] ?? null;
  const ownProjectID = ownProject?.id ?? '';

  const events = useQuery({
    queryKey: ['event-names', ownProjectID],
    queryFn: () => new AgentRayAPI(ownProjectID).eventNames(),
    enabled: !!ownProjectID,
    staleTime: 10 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const operations = useQuery({
    queryKey: ['operations', ownProjectID],
    queryFn: () => new AgentRayAPI(ownProjectID).operations(),
    enabled: !!ownProjectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
    retry: 1,
  });

  // The teammate the schedule would be armed on, in the user's own project —
  // never the demo's roster, which belongs to somebody else.
  const ownAgents = useQuery({
    queryKey: ['agents', ownProjectID],
    queryFn: () => new AgentRayAPI(ownProjectID).agents(),
    enabled: !!ownProjectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  const armTarget = (ownAgents.data?.agents ?? []).find((a) => a.enabled && a.is_default)
    ?? (ownAgents.data?.agents ?? []).find((a) => a.enabled)
    ?? (ownAgents.data?.agents ?? [])[0]
    ?? null;

  const input: TourInput = {
    ready: demo.resolved && own.resolved && (!ownProjectID || (!events.isPending && !operations.isPending)),
    hasDemo: !!demo.workspace,
    inDemo: !!project?.is_demo,
    demoName: demo.project?.name || demo.workspace?.name || '',
    ownProjectName: ownProject?.name || '',
    ownProjectCount: own.projects.length,
    ownEventNameCount: events.data?.names?.length ?? 0,
    ownScheduled: (operations.data?.operators ?? []).some((op) => op.kind === 'schedule' && op.enabled),
    agentName: agentName || armTarget?.name || '',
  };

  const steps = tourSteps(input);

  // Switching project from the tour is the same activation the project menu
  // does — the preference is written so a reload lands in the same place.
  function activate(next: Project | null, workspaceID: string, toast: string) {
    if (!next || next.id === project?.id) return;
    setSelectedWorkspaceID(workspaceID);
    setProjects(workspaceID === demo.workspace?.id ? (demo.project ? [demo.project] : []) : own.projects);
    setProject(next);
    writePreferredProjectID(next.id);
    setMessage(toast);
  }

  // First session lands in the demo rather than in an empty project of their
  // own. Both facts that gate it come from the server — there is a demo, and
  // their own project has never received an event — so a second device makes
  // the same call. The one local fact is whether they have ever chosen a
  // project in this browser: once they have, that choice is theirs and this
  // never fires again.
  const landed = useRef(false);
  useEffect(() => {
    if (landed.current) return;
    if (!input.ready || !input.hasDemo || input.inDemo) return;
    if (input.ownEventNameCount > 0) return;
    if (readPreferredProjectID()) return;
    if (!demo.project) return;
    landed.current = true;
    activate(demo.project, demo.workspace?.id ?? '', `Reading ${demo.project.name} — a real site someone else runs. You’re a viewer here.`);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [input.ready, input.hasDemo, input.inDemo, input.ownEventNameCount, demo.project?.id]);

  const arm = useMutation({
    mutationFn: () => {
      if (!ownProjectID) throw new Error('Open your own project first.');
      if (!armTarget) throw new Error('Hire a teammate first — there is nobody to put on a schedule.');
      return new AgentRayAPI(ownProjectID).createAgentTrigger(
        {
          name: TOUR_SCHEDULE_NAME,
          kind: 'schedule',
          enabled: true,
          cron: TOUR_SCHEDULE_CRON,
          prompt_template: TOUR_SCHEDULE_PROMPT,
          hmac_secret_name: '',
        },
        armTarget.id,
      );
    },
    onSuccess: async () => {
      setMessage(`${armTarget?.name ?? 'Your teammate'} runs every Monday at 09:00. Pause it any time in Operations.`);
      await queryClient.invalidateQueries({ queryKey: ['operations', ownProjectID] });
      await queryClient.invalidateQueries({ queryKey: ['agent-triggers', ownProjectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    input,
    steps,
    next: nextTourStep(input, steps),
    progress: tourProgress(steps),
    visible: showTour(input),
    demoProject: demo.project,
    demoWorkspaceID: demo.workspace?.id ?? '',
    ownProject,
    armAgentName: armTarget?.name ?? '',
    arming: arm.isPending,
    openDemo: () => activate(demo.project, demo.workspace?.id ?? '', `Reading ${demo.project?.name ?? 'the demo'} — you’re a viewer here.`),
    openOwn: () => activate(ownProject, own.workspace?.id ?? '', `Switched to ${ownProject?.name ?? 'your project'}.`),
    arm: () => { void arm.mutateAsync().catch(() => {}); },
  };
}

export type TourController = ReturnType<typeof useTour>;
export type { TourStep };
