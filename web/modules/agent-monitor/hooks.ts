'use client';

import { useQuery } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';

// useAgentMonitor fetches the project-wide rollup — every agent with its run
// counts, token/cost totals, and last activity. It powers agent health overview
// surfaces and the agents workspace rollup without needing per-agent code.
export function useAgentMonitor() {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['agent-monitor', projectID],
    queryFn: () => new AgentRayAPI(projectID!).agentMonitor(),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  return {
    agents: query.data?.agents ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
  };
}

// useAgentMonitorDetail fetches one agent's rollup plus its recent runs for the
// detailed health view shown inside the agent sheet. The per-run loop trace
// (tool + LLM calls) is fetched lazily on drill-in via useAgentRun.
export function useAgentMonitorDetail(agentID: string | null) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['agent-monitor-detail', projectID, agentID],
    queryFn: () => new AgentRayAPI(projectID!).agentMonitorDetail(agentID!),
    enabled: !!projectID && !!agentID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  return {
    agent: query.data?.agent,
    runs: query.data?.runs ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
  };
}
