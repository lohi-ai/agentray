'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type AgentScopes, type AgentTaskTiers, type WorkspaceModelTiersInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useWorkspaceModels() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const modelsQuery = useQuery({
    queryKey: ['workspace-models', projectID],
    queryFn: () => client().workspaceModels(),
    enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const saveModels = useMutation({
    mutationFn: (input: WorkspaceModelTiersInput) => client().updateWorkspaceModels(input),
    onSuccess: () => {
      setMessage('Workspace models saved');
      queryClient.invalidateQueries({ queryKey: ['workspace-models', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  const testModels = useMutation({
    mutationFn: () => client().testWorkspaceModels(),
    onSuccess: (res) => {
      if (res.ok) { setMessage('Connection OK'); return; }
      const failed = Object.entries(res.tiers ?? {})
        .filter(([, r]) => !r.ok)
        .map(([tier, r]) => `${tier}: ${r.error || 'failed'}`);
      setError(failed.length ? `Connection failed — ${failed.join('; ')}` : 'Connection failed');
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    models: modelsQuery.data?.config,
    modelsLoading: modelsQuery.isLoading,
    saveModels: (input: WorkspaceModelTiersInput) => saveModels.mutateAsync(input),
    testModels: () => testModels.mutateAsync(),
  };
}

export function useAgentCapabilities(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const capabilitiesQuery = useQuery({
    queryKey: ['agent-capabilities', projectID, agentID],
    queryFn: () => client().agentCapabilities(agentID),
    enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const saveCapabilities = useMutation({
    mutationFn: (scopes: AgentScopes) => client().updateAgentCapabilities(scopes, agentID),
    onSuccess: () => {
      setMessage('Capabilities saved');
      queryClient.invalidateQueries({ queryKey: ['agent-capabilities', projectID, agentID] });
      queryClient.invalidateQueries({ queryKey: ['agent-config', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    capabilities: capabilitiesQuery.data?.capabilities,
    capabilitiesLoading: capabilitiesQuery.isLoading,
    saveCapabilities: (scopes: AgentScopes) => saveCapabilities.mutateAsync(scopes),
  };
}

export function useAgentTaskTiers(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const tiersQuery = useQuery({
    queryKey: ['agent-task-tiers', projectID, agentID],
    queryFn: () => client().agentTaskTiers(agentID),
    enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const saveTiers = useMutation({
    mutationFn: (tiers: AgentTaskTiers) => client().updateAgentTaskTiers(tiers, agentID),
    onSuccess: () => {
      setMessage('Task tiers saved');
      queryClient.invalidateQueries({ queryKey: ['agent-task-tiers', projectID, agentID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    taskTiers: tiersQuery.data?.tiers,
    taskTiersLoading: tiersQuery.isLoading,
    saveTaskTiers: (tiers: AgentTaskTiers) => saveTiers.mutateAsync(tiers),
  };
}

export function useAgentRun(runID: string | null) {
  const projectID = useAuthStore((s) => s.project?.id);
  return useQuery({
    queryKey: ['agent-run', projectID, runID],
    queryFn: () => new AgentRayAPI(projectID!).agentRun(runID!),
    enabled: !!projectID && !!runID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
}
