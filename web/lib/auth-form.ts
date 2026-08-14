// Client-side auth-form rules. Keep these next to the screen so the first
// touch fails in the form, not as a silent toast after a dead fetch.

export type AuthMode = 'login' | 'signup';

export type AuthFormValues = {
  mode: AuthMode;
  email: string;
  name: string;
  password: string;
  workspaceName: string;
  projectName: string;
};

export type AuthField = 'email' | 'name' | 'password' | 'workspaceName' | 'projectName';

export type AuthFormIssue = {
  field: AuthField;
  message: string;
};

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function validateAuthForm(input: AuthFormValues): AuthFormIssue | null {
  const email = input.email.trim();
  if (!email) return { field: 'email', message: 'Enter your email.' };
  if (!EMAIL_RE.test(email)) return { field: 'email', message: 'Enter a valid email address.' };

  if (input.mode === 'signup') {
    if (!input.name.trim()) return { field: 'name', message: 'Enter your name.' };
  }

  if (!input.password) return { field: 'password', message: 'Enter your password.' };
  if (input.mode === 'signup' && input.password.length < 8) {
    return { field: 'password', message: 'Use at least 8 characters.' };
  }

  if (input.mode === 'signup') {
    if (!input.workspaceName.trim()) return { field: 'workspaceName', message: 'Name the workspace.' };
    if (!input.projectName.trim()) return { field: 'projectName', message: 'Name the first project.' };
  }
  return null;
}

// Map fetch / API failures into a first-touch sentence. Network errors are the
// common local-dev case (API not running on NEXT_PUBLIC_AGENTRAY_API_URL).
export function formatAuthError(err: unknown, apiHost = ''): string {
  const raw = err instanceof Error ? err.message : String(err ?? '');
  const lower = raw.toLowerCase();
  if (
    lower.includes('failed to fetch')
    || lower.includes('networkerror')
    || lower.includes('load failed')
    || lower.includes('network request failed')
    || lower.includes('err_connection')
  ) {
    return apiHost
      ? `Can’t reach AgentRay at ${apiHost}. Start the API and try again.`
      : 'Can’t reach AgentRay. Start the API and try again.';
  }
  if (!raw.trim()) return 'Authentication failed.';
  return raw;
}
