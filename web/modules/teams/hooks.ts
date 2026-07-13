'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type Team, type TeamCard, type TeamCardUpdate, type TeamMember } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useTeams() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const teamsQuery = useQuery({
    queryKey: ['teams', projectID],
    queryFn: () => client().teams(),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['teams', projectID] });
  const onErr = (e: Error) => setError(e.message);

  const createTeam = useMutation({
    mutationFn: (name: string) => client().createTeam(name),
    onSuccess: () => { setMessage('Team created'); invalidate(); },
    onError: onErr,
  });

  const removeTeam = useMutation({
    mutationFn: (id: string) => client().deleteTeam(id),
    onSuccess: () => { setMessage('Team deleted'); invalidate(); },
    onError: onErr,
  });

  return {
    teams: (teamsQuery.data?.teams ?? []) as Team[],
    teamsLoading: teamsQuery.isLoading,
    createTeam: (name: string) => createTeam.mutateAsync(name),
    removeTeam: (id: string) => removeTeam.mutateAsync(id),
  };
}

// useTeam loads one team's roster and board and exposes every mutation the
// detail page needs. All writes invalidate both the detail keys and the list
// (member/card counts show on the list cards).
export function useTeam(teamID: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID && !!teamID;

  const teamQuery = useQuery({
    queryKey: ['team', projectID, teamID],
    queryFn: () => client().team(teamID),
    enabled,
    refetchOnWindowFocus: false,
  });

  const cardsQuery = useQuery({
    queryKey: ['team-cards', projectID, teamID],
    queryFn: () => client().teamCards(teamID),
    enabled,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['team', projectID, teamID] });
    void queryClient.invalidateQueries({ queryKey: ['team-cards', projectID, teamID] });
    void queryClient.invalidateQueries({ queryKey: ['teams', projectID] });
  };
  const onErr = (e: Error) => setError(e.message);

  const updateTeam = useMutation({
    mutationFn: (input: { name?: string; lead_agent_id?: string }) => client().updateTeam(teamID, input),
    onSuccess: () => { setMessage('Team saved'); invalidate(); },
    onError: onErr,
  });

  const upsertMember = useMutation({
    mutationFn: (vars: { agentID: string; role?: string; position?: number }) =>
      client().upsertTeamMember(teamID, vars.agentID, { role: vars.role, position: vars.position }),
    onSuccess: () => { setMessage('Roster updated'); invalidate(); },
    onError: onErr,
  });

  const removeMember = useMutation({
    mutationFn: (agentID: string) => client().removeTeamMember(teamID, agentID),
    onSuccess: () => { setMessage('Member removed'); invalidate(); },
    onError: onErr,
  });

  const createCard = useMutation({
    mutationFn: (vars: { title: string; body?: string }) => client().createTeamCard(teamID, vars),
    onSuccess: () => { setMessage('Card added'); invalidate(); },
    onError: onErr,
  });

  const updateCard = useMutation({
    mutationFn: (vars: { cardID: string; input: TeamCardUpdate }) => client().updateTeamCard(teamID, vars.cardID, vars.input),
    onSuccess: () => invalidate(),
    onError: onErr,
  });

  const removeCard = useMutation({
    mutationFn: (cardID: string) => client().deleteTeamCard(teamID, cardID),
    onSuccess: () => { setMessage('Card deleted'); invalidate(); },
    onError: onErr,
  });

  return {
    team: (teamQuery.data?.team ?? null) as Team | null,
    members: (teamQuery.data?.members ?? []) as TeamMember[],
    cards: (cardsQuery.data?.cards ?? []) as TeamCard[],
    statuses: (cardsQuery.data?.statuses ?? ['backlog', 'doing', 'review', 'done']) as string[],
    isLoading: teamQuery.isLoading || cardsQuery.isLoading,
    notFound: teamQuery.isError,
    updateTeam: (input: { name?: string; lead_agent_id?: string }) => updateTeam.mutateAsync(input),
    upsertMember: (agentID: string, role?: string, position?: number) => upsertMember.mutateAsync({ agentID, role, position }),
    removeMember: (agentID: string) => removeMember.mutateAsync(agentID),
    createCard: (title: string, body?: string) => createCard.mutateAsync({ title, body }),
    updateCard: (cardID: string, input: TeamCardUpdate) => updateCard.mutateAsync({ cardID, input }),
    removeCard: (cardID: string) => removeCard.mutateAsync(cardID),
  };
}
