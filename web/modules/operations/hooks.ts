'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type Operator } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// hooks.ts — /operations reads one project-wide endpoint and writes through the
// per-agent trigger routes that already own the permission checks and the audit
// trail. There is no second way to configure a trigger; this surface only
// pauses, resumes, and starts a run by hand.

const OPERATIONS_KEY = 'operations';

export function useOperations() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: [OPERATIONS_KEY, projectID],
    queryFn: () => new AgentRayAPI(projectID!).operations(),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
    // One retry, not the default three. Three plus backoff is ~30s of spinner
    // before the "we couldn't load your operators" callout appears, which reads
    // as a hang; one still absorbs a transient blip.
    retry: 1,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: [OPERATIONS_KEY, projectID] });

  // Pause / resume writes the WHOLE trigger back, because that is the shape the
  // per-agent route takes. Every other field is echoed from the row on screen,
  // so flipping the switch cannot quietly blank a prompt or a cron.
  const setEnabled = useMutation({
    mutationFn: (v: { op: Operator; enabled: boolean }) =>
      new AgentRayAPI(projectID!).updateAgentTrigger(
        v.op.id,
        {
          name: v.op.name,
          enabled: v.enabled,
          cron: v.op.cron,
          prompt_template: v.op.prompt_template,
          hmac_secret_name: v.op.hmac_secret_name,
        },
        v.op.agent_id,
      ),
    onSuccess: (_data, v) => {
      setMessage(v.enabled ? 'Resumed. It runs on its own again.' : 'Paused. Nothing will start it until you resume.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  const runNow = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).runOperation(id),
    // "Queued", not "done": the run is dispatched onto the same queue the
    // schedule uses and finishes out of band. Claiming completion here would be
    // the page asserting an outcome it has not seen.
    onSuccess: () => {
      setMessage('Queued. Watch it in the run history.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    operators: query.data?.operators ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
    refetch: () => void query.refetch(),
    setEnabled: (op: Operator, enabled: boolean) => setEnabled.mutate({ op, enabled }),
    savingID: setEnabled.isPending ? setEnabled.variables?.op.id : undefined,
    runNow: (id: string) => runNow.mutate(id),
    runningID: runNow.isPending ? runNow.variables : undefined,
  };
}

export function useOperator(id: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: [OPERATIONS_KEY, projectID, id],
    queryFn: () => new AgentRayAPI(projectID!).operation(id),
    enabled: !!projectID && !!id,
    staleTime: 15 * 1000,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: [OPERATIONS_KEY, projectID, id] });
    queryClient.invalidateQueries({ queryKey: [OPERATIONS_KEY, projectID] });
  };

  const save = useMutation({
    mutationFn: (v: { op: Operator; name: string; cron: string; prompt: string; enabled: boolean }) =>
      new AgentRayAPI(projectID!).updateAgentTrigger(
        v.op.id,
        { name: v.name, enabled: v.enabled, cron: v.cron, prompt_template: v.prompt, hmac_secret_name: v.op.hmac_secret_name },
        v.op.agent_id,
      ),
    onSuccess: () => {
      setMessage('Saved.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  const runNow = useMutation({
    mutationFn: () => new AgentRayAPI(projectID!).runOperation(id),
    onSuccess: () => {
      setMessage('Queued. It will appear in the run history when it starts.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    operator: query.data?.operator ?? null,
    runs: query.data?.runs ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
    save: (v: { op: Operator; name: string; cron: string; prompt: string; enabled: boolean }) => save.mutate(v),
    saving: save.isPending,
    runNow: () => runNow.mutate(),
    running: runNow.isPending,
  };
}
