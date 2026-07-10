'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type AgentDefinitionDraft, type AgentSkillInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useAgentAuthoring(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const definitionQuery = useQuery({
    queryKey: ['agent-definition', projectID, agentID],
    queryFn: () => client().agentDefinition(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const skillsQuery = useQuery({
    queryKey: ['agent-skills', projectID, agentID],
    queryFn: () => client().agentSkills(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = (key: string) => queryClient.invalidateQueries({ queryKey: [key, projectID, agentID] });
  const onErr = (e: Error) => setError(e.message);

  const saveDefinition = useMutation({
    mutationFn: (vars: { soul_md: string; agents_md: string }) => client().updateAgentDefinition(vars.soul_md, vars.agents_md, agentID),
    onSuccess: () => { setMessage('Definition saved'); invalidate('agent-definition'); },
    onError: onErr,
  });

  const generateDefinitionDraft = useMutation({
    mutationFn: (prompt: string) => client().generateAgentDefinition(prompt, agentID),
    onSuccess: () => setMessage('Draft generated — review and save when ready'),
    onError: onErr,
  });

  const createSkill = useMutation({
    mutationFn: (input: AgentSkillInput) => client().createAgentSkill(input, agentID),
    onSuccess: () => { setMessage('Skill created'); invalidate('agent-skills'); },
    onError: onErr,
  });

  const updateSkill = useMutation({
    mutationFn: (vars: { id: string; input: AgentSkillInput }) => client().updateAgentSkill(vars.id, vars.input, agentID),
    onSuccess: () => { setMessage('Skill saved'); invalidate('agent-skills'); },
    onError: onErr,
  });

  const deleteSkill = useMutation({
    mutationFn: (id: string) => client().deleteAgentSkill(id, agentID),
    onSuccess: () => { setMessage('Skill deleted'); invalidate('agent-skills'); },
    onError: onErr,
  });

  const approveSkill = useMutation({
    mutationFn: (id: string) => client().approveAgentSkill(id, agentID),
    onSuccess: () => { setMessage('Skill approved'); invalidate('agent-skills'); },
    onError: onErr,
  });

  return {
    definition: definitionQuery.data?.definition,
    definitionLoading: definitionQuery.isLoading,
    definitionDraftPending: generateDefinitionDraft.isPending,
    skills: skillsQuery.data?.skills ?? [],
    skillsLoading: skillsQuery.isLoading,
    saveDefinition: (soul_md: string, agents_md: string) => saveDefinition.mutateAsync({ soul_md, agents_md }),
    generateDefinitionDraft: async (prompt: string): Promise<AgentDefinitionDraft> => {
      const res = await generateDefinitionDraft.mutateAsync(prompt);
      return res.definition;
    },
    createSkill: (input: AgentSkillInput) => createSkill.mutateAsync(input),
    updateSkill: (id: string, input: AgentSkillInput) => updateSkill.mutateAsync({ id, input }),
    deleteSkill: (id: string) => deleteSkill.mutateAsync(id),
    approveSkill: (id: string) => approveSkill.mutateAsync(id),
  };
}

// useAgentSkills is the read-only slice of the authoring skills query, for surfaces
// (like the chat composer's /slash menu) that only need to list an agent's skills
// and don't want the full authoring mutation set. Shares the same query key/cache
// as useAgentAuthoring so it's free when the editor has already loaded them.
export function useAgentSkills(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const skillsQuery = useQuery({
    queryKey: ['agent-skills', projectID, agentID],
    queryFn: () => new AgentRayAPI(projectID!).agentSkills(agentID),
    enabled: !!projectID && !!agentID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  return { skills: skillsQuery.data?.skills ?? [], loading: skillsQuery.isLoading };
}
