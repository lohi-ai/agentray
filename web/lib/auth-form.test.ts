import { describe, expect, it } from 'vitest';
import { formatAuthError, validateAuthForm } from './auth-form';

const login = {
  mode: 'login' as const,
  email: 'you@company.com',
  name: '',
  password: 'secret123',
  workspaceName: '',
  projectName: '',
};

const signup = {
  mode: 'signup' as const,
  email: 'you@company.com',
  name: 'Ada',
  password: 'secret123',
  workspaceName: 'Acme',
  projectName: 'Production',
};

describe('validateAuthForm', () => {
  it('accepts a complete login and signup', () => {
    expect(validateAuthForm(login)).toBeNull();
    expect(validateAuthForm(signup)).toBeNull();
  });

  it('requires email and a valid address before hitting the API', () => {
    expect(validateAuthForm({ ...login, email: '' })?.field).toBe('email');
    expect(validateAuthForm({ ...login, email: 'not-an-email' })?.message).toMatch(/valid email/i);
  });

  it('requires a password on login and 8+ characters on signup', () => {
    expect(validateAuthForm({ ...login, password: '' })?.field).toBe('password');
    expect(validateAuthForm({ ...signup, password: 'short' })?.message).toMatch(/8/);
  });

  it('requires name, workspace, and project only on signup', () => {
    expect(validateAuthForm({ ...signup, name: '' })?.field).toBe('name');
    expect(validateAuthForm({ ...signup, workspaceName: '' })?.field).toBe('workspaceName');
    expect(validateAuthForm({ ...signup, projectName: '' })?.field).toBe('projectName');
    expect(validateAuthForm({ ...login, name: '', workspaceName: '', projectName: '' })).toBeNull();
  });
});

describe('formatAuthError', () => {
  it('rewrites a dead API into a next-step sentence', () => {
    expect(formatAuthError(new Error('Failed to fetch'))).toMatch(/start the API/i);
    expect(formatAuthError(new Error('Failed to fetch'), 'http://localhost:8088')).toMatch(/localhost:8088/);
    expect(formatAuthError(new Error('NetworkError when attempting to fetch resource.'))).toMatch(/start the API/i);
  });

  it('keeps a server-provided message', () => {
    expect(formatAuthError(new Error('invalid email or password'))).toBe('invalid email or password');
  });
});
