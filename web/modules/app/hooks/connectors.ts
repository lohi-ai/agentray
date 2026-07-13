'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type ConnectorSyncInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// useConnectors drives the Data connectors settings tab: the project's
// configured external sources plus create / delete mutations. The DSN is
// write-only — it goes up in create and never comes back down.
export function useConnectors() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['connectors', projectID],
    queryFn: () => new AgentRayAPI(projectID!).connectors(),
    enabled: !!projectID,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['connectors', projectID] });

  const create = useMutation({
    mutationFn: (input: { name: string; kind: string; dsn: string }) => new AgentRayAPI(projectID!).createConnector(input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to add connector'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteConnector(id),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to delete connector'),
  });

  return {
    connectors: query.data?.connectors ?? [],
    kinds: query.data?.kinds ?? [],
    loading: query.isFetching,
    create,
    remove,
  };
}

// useConnectorSyncs lists one connector's table syncs (with run status) and
// exposes create / update / delete / run-now mutations. Run-now is synchronous
// on the API side, so its success invalidation already shows the run outcome.
export function useConnectorSyncs(connectorID: string | null) {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['connector-syncs', projectID, connectorID],
    queryFn: () => new AgentRayAPI(projectID!).connectorSyncs(connectorID!),
    enabled: !!projectID && !!connectorID,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['connector-syncs', projectID, connectorID] });

  const create = useMutation({
    mutationFn: (input: ConnectorSyncInput) => new AgentRayAPI(projectID!).createConnectorSync(connectorID!, input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to add sync'),
  });

  const update = useMutation({
    mutationFn: ({ id, input }: { id: string; input: ConnectorSyncInput }) => new AgentRayAPI(projectID!).updateConnectorSync(id, input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to update sync'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteConnectorSync(id),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to delete sync'),
  });

  const run = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).runConnectorSync(id),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to run sync'),
  });

  return {
    syncs: query.data?.syncs ?? [],
    loading: query.isFetching,
    create,
    update,
    remove,
    run,
  };
}

// useConnectorSchema lazily discovers the source's tables/columns — used by
// the add-sync dialog so the operator picks names instead of typing them.
export function useConnectorSchema(connectorID: string | null, enabled: boolean) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['connector-schema', projectID, connectorID],
    queryFn: () => new AgentRayAPI(projectID!).connectorSchema(connectorID!),
    enabled: !!projectID && !!connectorID && enabled,
    staleTime: 60 * 1000,
    retry: false,
  });
  return {
    tables: query.data?.tables ?? [],
    loading: query.isFetching,
    error: query.error instanceof Error ? query.error.message : null,
  };
}
