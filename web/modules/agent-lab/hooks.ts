'use client';

import { useCallback, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type LabStep } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';

// useLabCases manages an agent's saved Lab test cases: the list plus save /
// delete / verdict-override mutations. Cases are keyed by project + agent, so a
// per-agent Lab shows only its own cases. The default agent's id equals the
// project id; passing it through unchanged preserves single-agent behavior.
export function useLabCases(agentID: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const qc = useQueryClient();
  const key = ['agent-lab-cases', projectID, agentID];

  const query = useQuery({
    queryKey: key,
    queryFn: () => new AgentRayAPI(projectID!).labCases(agentID),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: key });

  const save = useMutation({
    mutationFn: (input: { name: string; input: string; expected: string }) =>
      new AgentRayAPI(projectID!).saveLabCase(input, agentID),
    onSuccess: invalidate,
  });

  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteLabCase(id, agentID),
    onSuccess: invalidate,
  });

  const setVerdict = useMutation({
    mutationFn: (vars: { id: string; status: string; runID?: string }) =>
      new AgentRayAPI(projectID!).setLabCaseVerdict(vars.id, vars.status, vars.runID ?? '', agentID),
    onSuccess: invalidate,
  });

  return {
    cases: query.data?.cases ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
    save,
    remove,
    setVerdict,
  };
}

export type ExplainPhase = 'idle' | 'paused' | 'running' | 'done' | 'error';

// useExplainRun drives the Lab's explain mode: the step-paused SSE stream. The
// server computes one step, emits it, and waits; the UI shows that step and the
// user clicks advance to compute the next. This is what makes the harness loop
// observable one turn at a time. start() opens the stream (it lives for the run's
// duration, updating state via its handlers); advance/stop/steer drive it.
export function useExplainRun(agentID: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const runRef = useRef<string>('');
  const [steps, setSteps] = useState<LabStep[]>([]);
  const [current, setCurrent] = useState(0);
  const [phase, setPhase] = useState<ExplainPhase>('idle');
  const [final, setFinal] = useState('');
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');

  const start = useCallback((input: string) => {
    if (!projectID || !input.trim()) return;
    runRef.current = '';
    setSteps([]);
    setCurrent(0);
    setFinal('');
    setStatus('');
    setError('');
    setPhase('running');
    void new AgentRayAPI(projectID).labExplainStream(input, {
      onRun: (id) => { runRef.current = id; },
      onStep: (s, c) => { setSteps(s); setCurrent(c); setPhase('paused'); },
      onDone: (s, st, f) => { setSteps((prev) => (s.length ? s : prev)); setCurrent(Math.max(0, (s.length || 1) - 1)); setStatus(st); setFinal(f); setPhase('done'); },
      onError: (msg) => { setError(msg); setPhase('error'); },
    }, agentID).catch((e: Error) => { setError(e.message); setPhase('error'); });
  }, [projectID, agentID]);

  const advance = useCallback(() => {
    if (!projectID || !runRef.current || phase !== 'paused') return;
    setPhase('running');
    void new AgentRayAPI(projectID).labAdvance(runRef.current, agentID).catch((e: Error) => { setError(e.message); setPhase('error'); });
  }, [projectID, agentID, phase]);

  const stop = useCallback(() => {
    if (!projectID || !runRef.current) return;
    void new AgentRayAPI(projectID).labStop(runRef.current, agentID).catch(() => {});
    setPhase('done');
  }, [projectID, agentID]);

  const steer = useCallback((message: string) => {
    if (!projectID || !runRef.current || !message.trim()) return;
    return new AgentRayAPI(projectID).labSteer(runRef.current, message, agentID);
  }, [projectID, agentID]);

  return { steps, current, setCurrent, phase, final, status, error, start, advance, stop, steer, runID: runRef };
}
