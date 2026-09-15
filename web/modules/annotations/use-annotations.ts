'use client';

import { useMemo } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { annotationWindow } from './annotations';

// AnnotationsController is the hook's contract: the windowed list plus the
// add/remove mutations the dialog drives. Named here so consumers import the
// type, not the hook's shape.
export type AnnotationsController = ReturnType<typeof useAnnotations>;

// AnnotationWindowInput is what a surface knows about its window: an explicit
// RFC3339 from/to (the overview's served range), or a filter's raw fields
// (from/to/hours) that resolve to a window at fetch time. The raw fields go
// into the query key unchanged, so the key stays stable and the impure "now"
// computation happens inside the query, not during render.
export type AnnotationWindowInput = { from?: string; to?: string; hours?: number };

// useAnnotations is the overlap-window read every temporal chart shares: the
// annotations whose instant or range intersects [from, to]. The window is
// part of the query key, so a range change refetches and an annotation
// outside the window is simply absent — the renderer never filters.
export function useAnnotations(window: AnnotationWindowInput | null) {
  const projectID = useAuthStore((s) => s.project?.id);
  const api = useMemo(() => (projectID ? new AgentRayAPI(projectID) : null), [projectID]);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ['annotations', projectID, window?.from, window?.to, window?.hours],
    queryFn: async () => {
      const to = window!.to ? new Date(window!.to) : new Date();
      const from = window!.from
        ? new Date(window!.from)
        : new Date(to.getTime() - (window!.hours ?? 24) * 3600_000);
      return (await api!.listAnnotations(annotationWindow(from, to))).annotations ?? [];
    },
    enabled: !!api && !!window,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['annotations', projectID] });

  const add = useMutation({
    mutationFn: (input: { label: string; kind: string; link?: string; starts_at: string; ends_at?: string }) =>
      api!.addAnnotation(input),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: (annotationID: string) => api!.deleteAnnotation(annotationID),
    onSuccess: invalidate,
  });

  return {
    annotations: query.data ?? [],
    loading: query.isLoading,
    add,
    remove,
  };
}
