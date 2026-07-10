import { create } from 'zustand';
import { toast } from 'sonner';
import type { AgentReplay, AuthState, Filters, InsightResult, Project, SavedQueryResult, Workspace } from '@/lib/api';
import { defaultFilters } from '@/lib/api';

type AuthSlice = {
  auth: AuthState | null;
  authChecked: boolean;
  workspaces: Workspace[];
  projects: Project[];
  selectedWorkspaceID: string;
  project: Project | null;
  applyAuth: (state: AuthState) => void;
  setAuthChecked: (value: boolean) => void;
  clearAuth: () => void;
  setWorkspaces: (workspaces: Workspace[]) => void;
  setProjects: (projects: Project[]) => void;
  setSelectedWorkspaceID: (id: string) => void;
  setProject: (project: Project | null) => void;
};

export const useAuthStore = create<AuthSlice>((set) => ({
  auth: null,
  authChecked: false,
  workspaces: [],
  projects: [],
  selectedWorkspaceID: '',
  project: null,
  applyAuth: (state) =>
    set({
      auth: state,
      workspaces: state.workspaces || [],
      projects: state.projects || [],
      selectedWorkspaceID: state.project?.workspace_id || state.workspaces?.[0]?.id || '',
      project: state.project || null,
    }),
  setAuthChecked: (value) => set({ authChecked: value }),
  clearAuth: () => set({ auth: null, workspaces: [], projects: [], selectedWorkspaceID: '', project: null }),
  setWorkspaces: (workspaces) => set({ workspaces }),
  setProjects: (projects) => set({ projects }),
  setSelectedWorkspaceID: (selectedWorkspaceID) => set({ selectedWorkspaceID }),
  setProject: (project) => set({ project }),
}));

type FiltersSlice = {
  filters: Filters;
  appliedFilters: Filters;
  setFilters: (filters: Filters) => void;
  commit: () => void;
  reset: () => void;
};

export const useFiltersStore = create<FiltersSlice>((set) => ({
  filters: defaultFilters,
  appliedFilters: defaultFilters,
  setFilters: (filters) => set({ filters }),
  commit: () => set((state) => ({ appliedFilters: { ...state.filters } })),
  reset: () => set({ filters: defaultFilters, appliedFilters: defaultFilters }),
}));

type UISlice = {
  message: string;
  error: string;
  selectedDashboardID: string;
  insight: InsightResult | null;
  replay: AgentReplay | null;
  sqlRows: Array<Record<string, unknown>>;
  savedResult: SavedQueryResult | null;
  setMessage: (value: string) => void;
  setError: (value: string) => void;
  setSelectedDashboardID: (id: string) => void;
  setInsight: (value: InsightResult | null) => void;
  setReplay: (value: AgentReplay | null) => void;
  setSQLRows: (rows: Array<Record<string, unknown>>) => void;
  setSavedResult: (value: SavedQueryResult | null) => void;
};

export const useUIStore = create<UISlice>((set) => ({
  message: '',
  error: '',
  selectedDashboardID: '',
  insight: null,
  replay: null,
  sqlRows: [],
  savedResult: null,
  setMessage: (message) => {
    set({ message });
    if (message) toast.success(message);
  },
  setError: (error) => {
    set({ error });
    if (error) toast.error(error);
  },
  setSelectedDashboardID: (selectedDashboardID) => set({ selectedDashboardID }),
  setInsight: (insight) => set({ insight }),
  setReplay: (replay) => set({ replay }),
  setSQLRows: (sqlRows) => set({ sqlRows }),
  setSavedResult: (savedResult) => set({ savedResult }),
}));
