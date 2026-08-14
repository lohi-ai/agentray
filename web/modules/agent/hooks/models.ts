'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type AgentScopes, type AgentTaskTiers, type WorkspaceModelTiersInput, type WorkspaceProviderInput } from '@/lib/api';
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
      queryClient.invalidateQueries({ queryKey: ['workspace-listed-models', projectID] });
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

  const providersQuery = useQuery({
    queryKey: ['workspace-providers', projectID],
    queryFn: () => client().workspaceProviders(),
    enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const listedQuery = useQuery({
    queryKey: ['workspace-listed-models', projectID],
    queryFn: () => client().listedWorkspaceModels(),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['workspace-models', projectID] });
    queryClient.invalidateQueries({ queryKey: ['workspace-providers', projectID] });
    queryClient.invalidateQueries({ queryKey: ['workspace-listed-models', projectID] });
  };

  const createProvider = useMutation({
    mutationFn: (input: WorkspaceProviderInput) => client().createWorkspaceProvider(input),
    onSuccess: () => { setMessage('Provider added'); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });

  const updateProvider = useMutation({
    mutationFn: ({ id, input }: { id: string; input: WorkspaceProviderInput }) =>
      client().updateWorkspaceProvider(id, input),
    onSuccess: () => { setMessage('Provider updated'); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });

  const deleteProvider = useMutation({
    mutationFn: (id: string) => client().deleteWorkspaceProvider(id),
    onSuccess: () => { setMessage('Provider removed'); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });

  return {
    models: modelsQuery.data?.config,
    modelsLoading: modelsQuery.isLoading,
    providers: providersQuery.data?.providers ?? modelsQuery.data?.config?.providers ?? [],
    listedModels: listedQuery.data?.models ?? [],
    listedErrors: listedQuery.data?.errors ?? [],
    listedLoading: listedQuery.isLoading,
    saveModels: (input: WorkspaceModelTiersInput) => saveModels.mutateAsync(input),
    testModels: () => testModels.mutateAsync(),
    createProvider: (input: WorkspaceProviderInput) => createProvider.mutateAsync(input),
    updateProvider: (id: string, input: WorkspaceProviderInput) => updateProvider.mutateAsync({ id, input }),
    deleteProvider: (id: string) => deleteProvider.mutateAsync(id),
    refreshListed: () => queryClient.invalidateQueries({ queryKey: ['workspace-listed-models', projectID] }),
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
