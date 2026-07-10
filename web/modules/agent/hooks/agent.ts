'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  AgentRayAPI,
  isSteered,
  type AgentConfigInput,
  type AgentChatStreamHandlers,
  type AgentChatStreamResult,
  type AgentChatTurn,
} from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useAgent() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const configQuery = useQuery({
    queryKey: ['agent-config', projectID],
    queryFn: () => client().agentConfig(),
    enabled,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const runsQuery = useQuery({
    queryKey: ['agent-runs', projectID],
    queryFn: () => client().agentRuns(),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const recsQuery = useQuery({
    queryKey: ['agent-recs', projectID],
    queryFn: () => client().agentRecommendations(),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const saveConfig = useMutation({
    mutationFn: (input: AgentConfigInput) => client().updateAgentConfig(input),
    onSuccess: () => {
      setMessage('Agent settings saved');
      queryClient.invalidateQueries({ queryKey: ['agent-config', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  const chat = useMutation({
    mutationFn: (message: string) => client().agentChat(message),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['agent-runs', projectID] }),
    onError: (e: Error) => setError(e.message),
  });

  const chatStream = async (
    message: string,
    handlers: AgentChatStreamHandlers = {},
    history: AgentChatTurn[] = [],
    opts: { sessionID?: string; mode?: 'steer' | 'followup'; agentID?: string; signal?: AbortSignal } = {},
  ): Promise<AgentChatStreamResult> => {
    try {
      const result = await client().agentChatStream(message, handlers, history, opts);
      if (!isSteered(result)) {
        queryClient.invalidateQueries({ queryKey: ['agent-runs', projectID] });
      }
      return result;
    } catch (e) {
      const err = e as Error;
      setError(err.message);
      throw err;
    }
  };

  // conversationSend posts a turn into a server-side conversation (the durable
  // store): the user turn is appended and the model history is derived server-side,
  // so NO client history is sent. Same streaming surface as chatStream. Used by the
  // conversation-backed chat path (DESIGN-CONVERSATION-STORE.md §9 step 4).
  const conversationSend = async (
    conversationID: string,
    message: string,
    handlers: AgentChatStreamHandlers = {},
    opts: { mode?: 'steer' | 'followup'; signal?: AbortSignal; agentID?: string } = {},
  ): Promise<AgentChatStreamResult> => {
    try {
      const result = await client().conversationSend(conversationID, message, handlers, opts);
      if (!isSteered(result)) {
        queryClient.invalidateQueries({ queryKey: ['agent-runs', projectID] });
      }
      return result;
    } catch (e) {
      const err = e as Error;
      setError(err.message);
      throw err;
    }
  };

  const triggerRun = useMutation({
    mutationFn: () => client().triggerAgentRun(),
    onSuccess: () => setMessage('Autonomous run queued'),
    onError: (e: Error) => setError(e.message),
  });

  const ack = useMutation({
    mutationFn: (vars: { id: string; status: 'accepted' | 'dismissed'; note?: string }) =>
      client().ackRecommendation(vars.id, vars.status, vars.note),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['agent-recs', projectID] }),
    onError: (e: Error) => setError(e.message),
  });

  return {
    config: configQuery.data?.config,
    configLoading: configQuery.isLoading,
    runs: runsQuery.data?.runs ?? [],
    recommendations: recsQuery.data?.recommendations ?? [],
    saveConfig: (input: AgentConfigInput) => saveConfig.mutateAsync(input),
    chat: (message: string) => chat.mutateAsync(message),
    chatStream,
    conversationSend,
    // Reattach a returning client to the latest run of a conversation; resolves
    // null when the session has no run yet (404). Carries the persisted tool trace
    // so a reloaded client can rebuild the step timeline it lost.
    sessionRun: (sessionID: string) =>
      client()
        .sessionRun(sessionID)
        .then((r) => ({ run: r.run, toolCalls: r.tool_calls ?? [] }))
        .catch(() => null),
    chatPending: chat.isPending,
    triggerRun: () => triggerRun.mutateAsync(),
    ackRecommendation: (id: string, status: 'accepted' | 'dismissed', note?: string) =>
      ack.mutateAsync({ id, status, note }),
  };
}
