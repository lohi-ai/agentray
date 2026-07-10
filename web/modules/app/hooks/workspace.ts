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
