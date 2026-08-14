'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type WorkspaceRole } from '@/lib/api';
import { useAuthStore, useFiltersStore, useUIStore } from '@/lib/app-state';

export function useWorkspaceUsage() {
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const projectID = useAuthStore((s) => s.project?.id);
  const appliedFilters = useFiltersStore((s) => s.appliedFilters);

  const query = useQuery({
    queryKey: ['workspace-usage', selectedWorkspaceID, appliedFilters],
    queryFn: () => new AgentRayAPI(projectID || '').workspaceUsage(selectedWorkspaceID, appliedFilters),
    enabled: !!selectedWorkspaceID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  return { usage: query.data?.usage ?? null, loading: query.isFetching };
}

// useWorkspacePlan is everything the three plan surfaces read: which plan the
// workspace is on, this calendar month's event count, and whether the deployment
// is the managed cloud at all. The month window is deliberately NOT the shared
// Filters range — a plan ceiling is monthly, so the meter must not move when the
// user changes a dashboard's date picker.
export function useWorkspacePlan() {
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const projectID = useAuthStore((s) => s.project?.id);
  const workspaces = useAuthStore((s) => s.workspaces);
  const hosted = useAuthStore((s) => s.auth?.hosted ?? false);
  const workspace = workspaces.find((w) => w.id === selectedWorkspaceID) ?? workspaces[0] ?? null;

  const query = useQuery({
    queryKey: ['workspace-month-usage', selectedWorkspaceID],
    queryFn: () => new AgentRayAPI(projectID || '').workspaceMonthUsage(selectedWorkspaceID),
    enabled: !!selectedWorkspaceID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  return {
    hosted,
    workspace,
    plan: workspace?.plan ?? 'free',
    usage: query.data?.usage ?? null,
    loading: query.isPending,
    // A meter that cannot load degrades to the text "Usage unavailable"; it
    // never blocks the shell and never shows a spinner where a number belongs.
    failed: query.isError,
  };
}

// useUpgradeRequest backs the interest form that stands in for a checkout while
// there is no payment processor. The POST writes a real row, so the button is
// never dead, and the latest request is read back so the sheet can say "you
// already asked" instead of inviting a duplicate.
export function useUpgradeRequest() {
  const queryClient = useQueryClient();
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['upgrade-request', selectedWorkspaceID],
    queryFn: () => new AgentRayAPI(projectID || '').latestUpgradeRequest(selectedWorkspaceID),
    enabled: !!selectedWorkspaceID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const mutation = useMutation({
    mutationFn: (input: { plan: string; email?: string; volume?: string; note?: string }) =>
      new AgentRayAPI(projectID || '').requestUpgrade(selectedWorkspaceID, input),
    onSuccess: async () => {
      setMessage('Thanks — we have your name.');
      await queryClient.invalidateQueries({ queryKey: ['upgrade-request', selectedWorkspaceID] });
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not send that. Try again.'),
  });

  return {
    request: query.data?.request ?? null,
    submit: (input: { plan: string; email?: string; volume?: string; note?: string }) => mutation.mutateAsync(input),
    submitting: mutation.isPending,
  };
}

export function useWorkspaceMembers() {
  const queryClient = useQueryClient();
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const api = new AgentRayAPI(projectID || '');

  const query = useQuery({
    queryKey: ['workspace-members', selectedWorkspaceID],
    queryFn: () => api.workspaceMembers(selectedWorkspaceID),
    enabled: !!selectedWorkspaceID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  async function invalidate() {
    await queryClient.invalidateQueries({ queryKey: ['workspace-members', selectedWorkspaceID] });
    await queryClient.invalidateQueries({ queryKey: ['workspace-audit-logs', selectedWorkspaceID] });
    await queryClient.invalidateQueries({ queryKey: ['me'] });
  }

  const addMutation = useMutation({
    mutationFn: (input: { email: string; role: WorkspaceRole }) => api.addWorkspaceMember(selectedWorkspaceID, input.email, input.role),
    onSuccess: async () => { setMessage('Workspace member updated.'); await invalidate(); },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to update member'),
  });

  const roleMutation = useMutation({
    mutationFn: (input: { userID: string; role: WorkspaceRole }) => api.updateWorkspaceMemberRole(selectedWorkspaceID, input.userID, input.role),
    onSuccess: async () => { setMessage('Member role updated.'); await invalidate(); },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to update member role'),
  });

  const removeMutation = useMutation({
    mutationFn: (userID: string) => api.removeWorkspaceMember(selectedWorkspaceID, userID),
    onSuccess: async () => { setMessage('Member removed.'); await invalidate(); },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to remove member'),
  });

  return {
    members: query.data?.members ?? [],
    loading: query.isFetching || addMutation.isPending || roleMutation.isPending || removeMutation.isPending,
    addMember: async (email: string, role: WorkspaceRole) => { await addMutation.mutateAsync({ email, role }); },
    updateMemberRole: async (userID: string, role: WorkspaceRole) => { await roleMutation.mutateAsync({ userID, role }); },
    removeMember: async (userID: string) => { await removeMutation.mutateAsync(userID); },
  };
}

export function useWorkspaceAuditLogs() {
  const selectedWorkspaceID = useAuthStore((s) => s.selectedWorkspaceID);
  const projectID = useAuthStore((s) => s.project?.id);

  const query = useQuery({
    queryKey: ['workspace-audit-logs', selectedWorkspaceID],
    queryFn: () => new AgentRayAPI(projectID || '').workspaceAuditLogs(selectedWorkspaceID, 10),
    enabled: !!selectedWorkspaceID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  return { logs: query.data?.logs ?? [], loading: query.isFetching };
}

export function useCurrentProject() {
  const queryClient = useQueryClient();
  const { auth, workspaces, projects, selectedWorkspaceID, project } = useAuthStore();
  const { setWorkspaces, setProjects, setSelectedWorkspaceID, setProject, applyAuth } = useAuthStore();
  const projectID = project?.id;
  const { setMessage, setError } = useUIStore();

  const api = projectID ? new AgentRayAPI(projectID) : new AgentRayAPI();

  const createWorkspaceMutation = useMutation({
    mutationFn: (name: string) => api.createWorkspace(name),
    onSuccess: (data) => {
      setWorkspaces([...workspaces, data.workspace]);
      setSelectedWorkspaceID(data.workspace.id);
      setProjects([]);
      setProject(null);
      setMessage('Workspace created.');
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to create workspace'),
  });

  const updateWorkspaceMutation = useMutation({
    mutationFn: (name: string) => api.updateWorkspace(selectedWorkspaceID, name),
    onSuccess: async (data) => {
      setWorkspaces(workspaces.map((w) => (w.id === data.workspace.id ? data.workspace : w)));
      setMessage('Workspace updated.');
      await queryClient.invalidateQueries({ queryKey: ['workspace-audit-logs', selectedWorkspaceID] });
    },
  });

  const selectWorkspaceMutation = useMutation({
    mutationFn: async (workspaceID: string) => {
      setSelectedWorkspaceID(workspaceID);
      return api.workspaceProjects(workspaceID);
    },
    onSuccess: (data) => {
      setProjects(data.projects);
      setProject(data.projects[0] || null);
    },
  });

  const createProjectMutation = useMutation({
    mutationFn: (name: string) => {
      if (!selectedWorkspaceID) throw new Error('Create or select a workspace first.');
      return api.createWorkspaceProject(selectedWorkspaceID, name);
    },
    onSuccess: async (data) => {
      setProjects([...projects, data.project]);
      setProject(data.project);
      setMessage('Project created and connected.');
      await queryClient.invalidateQueries({ queryKey: ['console', projectID] });
      await queryClient.invalidateQueries({ queryKey: ['workspace-audit-logs', selectedWorkspaceID] });
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to create project'),
  });

  const updateProjectMutation = useMutation({
    mutationFn: (name: string) => api.updateProject(project!.id, name),
    onSuccess: async (data) => {
      setProject(data.project);
      setProjects(projects.map((p) => (p.id === data.project.id ? data.project : p)));
      setMessage('Project updated.');
      await queryClient.invalidateQueries({ queryKey: ['workspace-audit-logs', selectedWorkspaceID] });
    },
  });

  const updateUserMutation = useMutation({
    mutationFn: (name: string) => api.updateUser(name),
    onSuccess: (state) => { applyAuth(state); setMessage('Profile updated.'); },
  });

  const rotateKeyMutation = useMutation({
    mutationFn: () => api.rotateKey(project!.id),
    onSuccess: async (data) => {
      setProject(data.project);
      setProjects(projects.map((p) => (p.id === data.project.id ? data.project : p)));
      setMessage('API key rotated. The new key is active here.');
      await queryClient.invalidateQueries({ queryKey: ['workspace-audit-logs', selectedWorkspaceID] });
    },
  });

  return {
    auth,
    workspaces,
    projects,
    selectedWorkspaceID,
    project,
    selectWorkspace: async (id: string) => { await selectWorkspaceMutation.mutateAsync(id); },
    selectProject: async (id: string) => {
      const next = projects.find((p) => p.id === id) || null;
      if (next) setProject(next);
    },
    createWorkspace: async (name: string) => { await createWorkspaceMutation.mutateAsync(name); },
    updateWorkspace: async (name: string) => { await updateWorkspaceMutation.mutateAsync(name); },
    createProject: async (name: string) => { await createProjectMutation.mutateAsync(name); },
    updateProject: async (name: string) => { await updateProjectMutation.mutateAsync(name); },
    updateUser: async (name: string) => { await updateUserMutation.mutateAsync(name); },
    rotateKey: async () => { await rotateKeyMutation.mutateAsync(); },
  };
}
