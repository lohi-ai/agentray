'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// hooks.ts — /prototypes reads the plural endpoint and writes only the two acts
// that are the owner's alone: committing to a number before the data arrives,
// and deciding what the result meant. Creating a prototype stays in chat, where
// the agent designs the test — a threshold the owner typed into a form is a
// number they can also quietly retype.

const PROTOTYPES_KEY = 'prototypes';

function useInvalidate(projectID?: string) {
  const queryClient = useQueryClient();
  return () => {
    queryClient.invalidateQueries({ queryKey: [PROTOTYPES_KEY, projectID] });
    // /start reads the same rows through /api/validation/status. Leaving it
    // stale means committing here and watching the onboarding page keep asking
    // for a commitment already made.
    queryClient.invalidateQueries({ queryKey: ['validation-status', projectID] });
  };
}

export function usePrototypes() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const invalidate = useInvalidate(projectID);

  const query = useQuery({
    queryKey: [PROTOTYPES_KEY, projectID],
    queryFn: () => new AgentRayAPI(projectID!).validationTests(),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
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

export function usePrototype(id: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const invalidateList = useInvalidate(projectID);

  const query = useQuery({
    queryKey: [PROTOTYPES_KEY, projectID, id],
    queryFn: () => new AgentRayAPI(projectID!).validationTest(id),
    enabled: !!projectID && !!id,
    staleTime: 15 * 1000,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: [PROTOTYPES_KEY, projectID, id] });
    invalidateList();
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

  return {
    test: query.data?.test ?? null,
    waitlistCount: query.data?.waitlist_count ?? 0,
    isLoading: query.isLoading,
    error: query.error as Error | null,
    commit: () => commit.mutate(),
    committing: commit.isPending,
    decide: (status: string, note: string) => decide.mutate({ status, note }),
    deciding: decide.isPending,
  };
}

export function useWaitlist(enabled: boolean) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['waitlist', projectID],
    queryFn: () => new AgentRayAPI(projectID!).waitlistSignups(50),
    enabled: !!projectID && enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });
  return {
    signups: query.data?.signups ?? [],
    count: query.data?.count ?? 0,
    isLoading: query.isLoading,
  };
}
