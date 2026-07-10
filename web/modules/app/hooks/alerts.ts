'use client';

import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  AgentRayAPI,
  type AlertChannelInput,
  type AlertRuleInput,
} from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// useAlertRules drives the Alerts page: the project's rule list plus create /
// update / delete / toggle mutations, each invalidating the list so the table
// reflects the change without a manual refetch.
export function useAlertRules() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['alert-rules', projectID],
    queryFn: () => new AgentRayAPI(projectID!).alertRules(),
    enabled: !!projectID,
  });

  useEffect(() => {
    if (query.error) setError(query.error instanceof Error ? query.error.message : 'Unable to load alerts');
  }, [query.error, setError]);

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['alert-rules', projectID] });

  const create = useMutation({
    mutationFn: (input: AlertRuleInput) => new AgentRayAPI(projectID!).createAlertRule(input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to create alert'),
  });

  const update = useMutation({
    mutationFn: ({ id, input }: { id: string; input: AlertRuleInput }) => new AgentRayAPI(projectID!).updateAlertRule(id, input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to update alert'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteAlertRule(id),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to delete alert'),
  });

  return {
    rules: query.data?.rules ?? [],
    loading: query.isFetching,
    create,
    update,
    remove,
  };
}

// useAlertChannels lists the workspace notification channels an alert rule can
// target and lets the page add or drop one.
export function useAlertChannels() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['alert-channels', projectID],
    queryFn: () => new AgentRayAPI(projectID!).alertChannels(),
    enabled: !!projectID,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['alert-channels', projectID] });

  const create = useMutation({
    mutationFn: (input: AlertChannelInput) => new AgentRayAPI(projectID!).createAlertChannel(input),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to add channel'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteAlertChannel(id),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to remove channel'),
  });

  return { channels: query.data?.channels ?? [], loading: query.isFetching, create, remove };
}
