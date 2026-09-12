'use client';

import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, newIdempotencyKey } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// hooks.ts — /plans reads findings through the list_findings op (keyset pages,
// so the full history is resumable) and experiments through the existing
// validation REST surface, which returns the full measured row. Writes split
// the same way the backend does: commit/decide are session-only owner acts,
// outcome/abandon go through the revision-checked ops.

const PLANS_KEY = 'plans';
const FINDINGS_KEY = 'findings';

function useInvalidatePlans(projectID?: string) {
  const queryClient = useQueryClient();
  return () => {
    queryClient.invalidateQueries({ queryKey: [PLANS_KEY, projectID] });
    queryClient.invalidateQueries({ queryKey: [FINDINGS_KEY, projectID] });
    // /prototypes and /start read the same validation_tests rows — leaving
    // them stale means committing here and watching the other surfaces keep
    // asking for a commitment already made.
    queryClient.invalidateQueries({ queryKey: ['prototypes', projectID] });
    queryClient.invalidateQueries({ queryKey: ['validation-status', projectID] });
  };
}

export function useFindings() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const invalidate = useInvalidatePlans(projectID);

  const query = useInfiniteQuery({
    queryKey: [FINDINGS_KEY, projectID],
    queryFn: ({ pageParam }) => new AgentRayAPI(projectID!).listFindings({ cursor: pageParam, limit: 50 }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
    retry: 1,
  });

  const ack = useMutation({
    mutationFn: (v: { id: string; status: 'accepted' | 'dismissed' }) =>
      new AgentRayAPI(projectID!).ackRecommendation(v.id, v.status),
    onSuccess: (_r, v) => {
      setMessage(v.status === 'accepted' ? 'Marked as accepted.' : 'Dismissed.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    findings: query.data?.pages.flatMap((p) => p.findings ?? []) ?? [],
    hasMore: query.hasNextPage,
    loadMore: () => void query.fetchNextPage(),
    loadingMore: query.isFetchingNextPage,
    isLoading: query.isLoading,
    error: query.error as Error | null,
    refetch: () => void query.refetch(),
    ack: (id: string, status: 'accepted' | 'dismissed') => ack.mutate({ id, status }),
    ackingID: ack.isPending ? ack.variables?.id : undefined,
  };
}

export function useExperiments() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const invalidate = useInvalidatePlans(projectID);

  const query = useQuery({
    queryKey: [PLANS_KEY, projectID, 'experiments'],
    queryFn: () => new AgentRayAPI(projectID!).validationTests(),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
    retry: 1,
  });

  const commit = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).commitValidationTest(id),
    onSuccess: () => {
      setMessage('Committed. The number is settled before the data arrives.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  return {
    tests: query.data?.tests ?? [],
    total: query.data?.total ?? 0,
    truncated: query.data?.truncated ?? false,
    waitlistCount: query.data?.waitlist_count ?? 0,
    isLoading: query.isLoading,
    error: query.error as Error | null,
    refetch: () => void query.refetch(),
    commit: (id: string) => commit.mutate(id),
    committingID: commit.isPending ? commit.variables : undefined,
  };
}

export function useExperiment(id: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const invalidateList = useInvalidatePlans(projectID);

  const query = useQuery({
    queryKey: [PLANS_KEY, projectID, 'experiment', id],
    queryFn: () => new AgentRayAPI(projectID!).validationTest(id),
    enabled: !!projectID && !!id,
    staleTime: 15 * 1000,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: [PLANS_KEY, projectID, 'experiment', id] });
    invalidateList();
  };
  // A revision conflict means the row changed under this page — the honest
  // recovery is to reload it and say so, not to leave the stale copy on
  // screen behind an error toast.
  const onWriteError = (e: Error) => {
    if (e.message.includes('revision conflict')) {
      invalidate();
      setError('This experiment changed since you opened it — reloaded the latest.');
      return;
    }
    setError(e.message);
  };

  const commit = useMutation({
    mutationFn: () => new AgentRayAPI(projectID!).commitValidationTest(id),
    onSuccess: () => {
      setMessage('Committed. The number is settled before the data arrives.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  const decide = useMutation({
    mutationFn: (v: { status: string; note: string }) => new AgentRayAPI(projectID!).decideValidationTest(id, v.status, v.note),
    onSuccess: () => {
      setMessage('Recorded. The why is the part worth reading a month from now.');
      invalidate();
    },
    onError: (e: Error) => setError(e.message),
  });

  // Abandon a PROPOSED experiment through the revision-checked op — the one
  // additive transition the ops surface owns. Committed tests close through
  // decide; owner agreement is a human act.
  const abandon = useMutation({
    mutationFn: (v: { reason: string }) =>
      new AgentRayAPI(projectID!).abandonTestOp({
        test_id: id,
        revision: query.data?.test.revision ?? 1,
        reason: v.reason,
        idempotency_key: newIdempotencyKey(),
      }),
    onSuccess: () => {
      setMessage('Abandoned. The proposal is closed; the record stays for history.');
      invalidate();
    },
    onError: onWriteError,
  });

  // recordOutcome appends one observation to the append-only outcome list —
  // evidence, not a verdict. The pass/fail/abandon decision stays a separate
  // human act.
  const recordOutcome = useMutation({
    mutationFn: (v: { value: number; unit: string; window: string; evidence_ref: string }) =>
      new AgentRayAPI(projectID!).recordOutcome({
        test_id: id,
        revision: query.data?.test.revision ?? 1,
        ...v,
        idempotency_key: newIdempotencyKey(),
      }),
    onSuccess: () => {
      setMessage('Outcome recorded. The decision is still yours — this entry is evidence, not a verdict.');
      invalidate();
    },
    onError: onWriteError,
  });

  return {
    test: query.data?.test ?? null,
    waitlistCount: query.data?.waitlist_count ?? 0,
    isLoading: query.isLoading,
    error: query.error as Error | null,
    commit: () => commit.mutate(),
    committing: commit.isPending,
    decide: (status: string, note: string) => decide.mutate({ status, note }),
    deciding: decide.isPending,
    abandon: (reason: string) => abandon.mutate({ reason }),
    abandoning: abandon.isPending,
    recordOutcome: (v: { value: number; unit: string; window: string; evidence_ref: string }) => recordOutcome.mutate(v),
    recording: recordOutcome.isPending,
  };
}
