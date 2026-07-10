'use client';

import { useEffect } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useAuth() {
  const { auth, authChecked } = useAuthStore();
  const applyAuth = useAuthStore((s) => s.applyAuth);
  const setAuthChecked = useAuthStore((s) => s.setAuthChecked);
  const clearAuth = useAuthStore((s) => s.clearAuth);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);

  const meQuery = useQuery({
    queryKey: ['me'],
    queryFn: () => new AgentRayAPI().me(),
    retry: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });

  useEffect(() => {
    if (meQuery.isSuccess && meQuery.data) applyAuth(meQuery.data);
  }, [meQuery.isSuccess, meQuery.data, applyAuth]);

  useEffect(() => {
    if (meQuery.isSuccess || meQuery.isError) setAuthChecked(true);
  }, [meQuery.isSuccess, meQuery.isError, setAuthChecked]);

  const loginMutation = useMutation({
    mutationFn: (input: { mode: 'login' | 'signup'; email: string; name: string; password: string; workspaceName: string; projectName: string }) => {
      const client = new AgentRayAPI();
      return input.mode === 'signup'
        ? client.signup({ email: input.email, name: input.name, password: input.password, workspace_name: input.workspaceName, project_name: input.projectName })
        : client.login(input.email, input.password);
    },
    onSuccess: (state, input) => {
      applyAuth(state);
      setAuthChecked(true);
      setMessage(input.mode === 'signup' ? 'Workspace created. Welcome to AgentRay.' : 'Logged in.');
    },
    onError: (err) => {
      setAuthChecked(true);
      setError(err instanceof Error ? err.message : 'Authentication failed');
    },
  });

  const logoutMutation = useMutation({
    mutationFn: () => new AgentRayAPI().logout(),
    onSuccess: () => {
      clearAuth();
      setMessage('Logged out.');
    },
  });

  return {
    auth,
    authChecked,
    loading: loginMutation.isPending || logoutMutation.isPending,
    submitAuth: async (input: Parameters<typeof loginMutation.mutateAsync>[0]) => {
      await loginMutation.mutateAsync(input);
    },
    logout: async () => {
      await logoutMutation.mutateAsync();
    },
  };
}

export function useUser() {
  return useAuthStore((s) => s.auth?.user ?? null);
}
