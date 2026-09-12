'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, apiErrorMessage, newIdempotencyKey, type ConnectorSyncInput } from '@/lib/api';
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
    mutationFn: (input: { name: string; kind: string; dsn: string; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).createConnector(input, { idempotencyKey: input.idempotencyKey }),
    onSuccess: invalidate,
    onError: (e) => setError(apiErrorMessage(e, 'Unable to add connector')),
  });

  // remove is the reversible archive: the connector leaves the list, its syncs
  // pause, and the row + credential + landed data are kept for restore.
  const remove = useMutation({
    mutationFn: (input: { id: string; revision?: number; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).deleteConnector(input.id, { revision: input.revision, idempotencyKey: input.idempotencyKey }),
    onSuccess: invalidate,
    onError: (e) => setError(apiErrorMessage(e, 'Unable to delete connector')),
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
    onError: (e) => setError(apiErrorMessage(e, 'Unable to delete sync')),
  });

  const run = useMutation({
    mutationFn: (input: { id: string; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).runConnectorSync(input.id, { idempotencyKey: input.idempotencyKey }),
    onSuccess: invalidate,
    onError: (e) => setError(apiErrorMessage(e, 'Unable to run sync')),
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

// useDatasetPreview reads one sync's landed rows through the dataset_preview
// op — deduped FINAL rows with the soft-delete filter applied, plus the
// freshness block (last success vs last attempt vs landed watermark) and the
// standing warnings (grain, deletion coverage, replacement tie-break). run_sql
// applies the same soft-delete predicate inside its scoped CTE; this op adds
// the sync metadata and warnings around the rows.
export function useDatasetPreview(syncID: string | null) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['dataset-preview', projectID, syncID],
    queryFn: () => new AgentRayAPI(projectID!).datasetPreview(syncID!),
    enabled: !!projectID && !!syncID,
    staleTime: 30 * 1000,
    retry: false,
  });
  return {
    preview: query.data ?? null,
    loading: query.isFetching,
    error: query.error instanceof Error ? query.error.message : null,
  };
}
