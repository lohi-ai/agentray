'use client';

import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, APIError, apiErrorMessage, newIdempotencyKey, type ConnectorSync, type ConnectorSyncInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export type ReadinessSync = ConnectorSync & { connector_name: string };

const connectorSyncsKey = (projectID: string | undefined, connectorID: string | null) => ['connector-syncs', projectID, connectorID] as const;

function isConnectorSyncActive(sync: ConnectorSync): boolean {
  return sync.readiness?.state === 'syncing'
    || sync.latest_run?.status === 'queued'
    || sync.latest_run?.status === 'running';
}

function connectorSyncsQuery(projectID: string | undefined, connectorID: string) {
  return {
    queryKey: connectorSyncsKey(projectID, connectorID),
    queryFn: () => new AgentRayAPI(projectID!).connectorSyncs(connectorID),
    enabled: !!projectID,
    refetchInterval: (query: { state: { data?: { syncs?: ConnectorSync[] } } }) =>
      (query.state.data?.syncs ?? []).some(isConnectorSyncActive) ? 2000 : false,
  };
}

// One read model for evidence consumers that need readiness across connectors.
// Each connector owns one cache key and one poller, so the settings summary and
// selected table consume the same source_status response instead of issuing
// parallel aggregate/detail requests. A 403 remains distinguishable from empty.
export function useSourceReadinessOverview(connectorIDs?: Array<{ id: string; name: string }>) {
  const projectID = useAuthStore((s) => s.project?.id);
  const connectorList = useQuery({
    queryKey: ['connectors', projectID],
    queryFn: () => new AgentRayAPI(projectID!).connectors(),
    enabled: !!projectID && connectorIDs === undefined,
  });
  const connectors = connectorIDs ?? connectorList.data?.connectors.map((connector) => ({ id: connector.id, name: connector.name })) ?? [];
  const statusQueries = useQueries({
    queries: connectors.map((connector) => connectorSyncsQuery(projectID, connector.id)),
  });
  const errors = [connectorList.error, ...statusQueries.map((query) => query.error)].filter((error): error is Error => error instanceof Error);
  const denied = errors.some((error) => error instanceof APIError && error.status === 403);
  const error = errors.find((candidate) => !(candidate instanceof APIError && candidate.status === 403)) ?? null;
  const loadingByConnector = Object.fromEntries(connectors.map((connector, index) => [connector.id, statusQueries[index]?.isFetching ?? false]));
  const syncs = connectors.flatMap((connector, index) =>
    (statusQueries[index]?.data?.syncs ?? []).map((sync) => ({ ...sync, connector_name: connector.name })),
  );
  return {
    syncs,
    loading: connectorList.isFetching || statusQueries.some((query) => query.isFetching),
    loadingByConnector,
    denied,
    error,
  };
}

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

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['connectors', projectID] });
  };

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

// useConnectorSyncs exposes mutations for the selected connector. Its rows are
// supplied by useSourceReadinessOverview, the sole source_status query owner,
// so the summary and detail table cannot start duplicate polling loops.
export function useConnectorSyncs(connectorID: string | null, status: { syncs: ConnectorSync[]; loading: boolean }) {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const invalidate = () => {
    // The shared query follows queued/running receipts as well as readiness, so
    // a just-enqueued run cannot strand either the summary or detail table.
    void queryClient.invalidateQueries({ queryKey: connectorSyncsKey(projectID, connectorID) });
    // The preview is a separate query with its own 30s staleTime. A landed run,
    // a re-pointed key/cursor column or a pause all change what it would show,
    // and without this it keeps serving the rows from before the change.
    void queryClient.invalidateQueries({ queryKey: ['dataset-preview', projectID] });
  };

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

  // run-now enqueues a durable run and returns its receipt; the poll above
  // tracks it to a terminal state. Typed conflict means the sync is paused;
  // retryable means engine capacity.
  const run = useMutation({
    mutationFn: (input: { id: string; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).runConnectorSync(input.id, { idempotencyKey: input.idempotencyKey }),
    onSuccess: invalidate,
    onError: (e) => setError(apiErrorMessage(e, 'Unable to run sync')),
  });

  const cancel = useMutation({
    mutationFn: (runID: string) => new AgentRayAPI(projectID!).cancelConnectorRun(runID),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to cancel run'),
  });

  const setEnabled = useMutation({
    mutationFn: ({ sync, enabled }: { sync: ConnectorSync; enabled: boolean }) =>
      new AgentRayAPI(projectID!).setConnectorSyncEnabled(sync, enabled),
    onSuccess: invalidate,
    onError: (e) => {
      if (e instanceof APIError && e.kind === 'conflict') void invalidate();
      setError(apiErrorMessage(e, 'Unable to update sync'));
    },
  });

  return {
    syncs: status.syncs,
    loading: status.loading,
    create,
    update,
    remove,
    run,
    cancel,
    setEnabled,
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
// op — deduped rows with the soft-delete filter applied, plus the
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
