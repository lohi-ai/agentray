'use client';

import { useEffect, useRef, useState } from 'react';
import { Waypoints } from 'lucide-react';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { TextInput, type TextInputProps } from '@astryxdesign/core/TextInput';
import { validateAuthForm, type AuthField, type AuthMode } from '@/lib/auth-form';

// The first project a signup names is NOT the project they land in: CreateAccount
// inserts Demo first so the first session opens on a populated funnel
// (internal/dataplane/store/auth.go:136). So the field is hidden and the constant
// is sent instead — validateAuthForm's projectName rule still passes, and
// POST /api/auth/signup is unchanged. Project is renameable from the chrome.
const HIDDEN_PROJECT_NAME = 'Production';

// TextInput spreads unknown props straight onto the <input> (TextInput.js:146),
// but `autoComplete` is missing from TextInputProps because BaseProps extends
// React.HTMLAttributes, not InputHTMLAttributes. Without it a password manager
// offers the *existing* password on the signup form instead of generating one,
// so pass it through a narrow typed spread rather than dropping it.
function autoComplete(value: string): Partial<TextInputProps> {
  return { autoComplete: value } as Partial<TextInputProps>;
}

export function AuthScreen({
  loading,
  error,
  onSubmit,
  onModeChange,
}: {
  loading: boolean;
  error: string;
  onSubmit: (input: { mode: AuthMode; email: string; name: string; password: string; workspaceName: string; projectName: string }) => Promise<void>;
  onModeChange?: () => void;
}) {
  // Sign up first: a stranger's job is to create a workspace, not guess they already have one.
  const [mode, setMode] = useState<AuthMode>('signup');
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [workspaceName, setWorkspaceName] = useState('My workspace');
  const [issue, setIssue] = useState<{ field: AuthField; message: string } | null>(null);
  const emailRef = useRef<HTMLInputElement>(null);
  const isSignup = mode === 'signup';

  // Autofocus only where a keyboard is already there. On a touch device the
  // popped keyboard halves an already-tall card.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    if (window.matchMedia?.('(pointer: fine)').matches) emailRef.current?.focus();
  }, []);

  function fieldStatus(field: AuthField) {
    return issue?.field === field ? { type: 'error' as const, message: issue.message } : undefined;
  }

  // Switching mode clears the field error here and asks the gate to drop its API
  // banner — otherwise a failed login stays nailed above "Create your workspace".
  function switchMode(next: AuthMode) {
    setMode(next);
    setIssue(null);
    onModeChange?.();
  }

  return (
    <main className="min-h-dvh bg-[var(--color-background-body)] px-5 py-8">
      {/* min-height, never a fixed 100dvh: a centered fixed-height flex box spills
          its overflow above the top edge, where no scroll can reach it. This grows
          instead — 4rem is the py-8 the wrapper already spends. */}
      <Center axis="both" width="100%" className="min-h-[calc(100dvh-4rem)]">
        <VStack gap={4} align="center" className="w-full max-w-[400px]">
          <VStack gap={2} align="center">
            <span
              className="grid size-8 flex-none place-items-center rounded-[var(--radius-lg)]"
              style={{ background: 'color-mix(in srgb, var(--primary) 16%, transparent)', color: 'var(--primary)' }}
              aria-hidden
            >
              <Waypoints size={18} />
            </span>
            <Text type="body" weight="medium">AgentRay</Text>
          </VStack>

          <Card padding={6} width="100%">
            <VStack gap={4} align="stretch">
              <VStack gap={1} align="start">
                <Heading level={1}>{isSignup ? 'Create your workspace' : 'Welcome back'}</Heading>
                <Text type="supporting">
                  {isSignup
                    ? 'You’ll land in a live demo funnel — connect your data after.'
                    : 'Sign in to keep going.'}
                </Text>
              </VStack>

              {error ? <Banner status="error" title={error} /> : null}

              {/* noValidate: validateAuthForm is the only validator. Without it
                  the browser's own typeMismatch on type="email" blocks submit
                  and shows an unstyled native bubble, so the inline TextInput
                  status this screen is built around never renders. (isRequired
                  is aria-only in Astryx, so the empty case already reached us —
                  only the malformed-email case was being swallowed.) */}
              <form
                noValidate
                onSubmit={(event) => {
                  event.preventDefault();
                  const next = validateAuthForm({ mode, email, name, password, workspaceName, projectName: HIDDEN_PROJECT_NAME });
                  setIssue(next);
                  if (next) return;
                  void onSubmit({ mode, email, name, password, workspaceName, projectName: HIDDEN_PROJECT_NAME });
                }}
              >
                {/* Astryx lg is 36px and md is 32px, both under the 44px touch
                    target DESIGN.md asks for. className lands on the input
                    wrapper that carries the StyleX height, so min-h-11 wins. */}
                <VStack gap={3} align="stretch">
                  <TextInput
                    ref={emailRef}
                    label="Email"
                    type="email"
                    value={email}
                    onChange={(v) => { setEmail(v); setIssue(null); }}
                    htmlName="email"
                    placeholder="you@company.com"
                    isRequired
                    status={fieldStatus('email')}
                    className="min-h-11"
                    {...autoComplete('username')}
                  />
                  {isSignup ? (
                    <TextInput
                      label="Full name"
                      value={name}
                      onChange={(v) => { setName(v); setIssue(null); }}
                      htmlName="name"
                      placeholder="Ada Lovelace"
                      isRequired
                      status={fieldStatus('name')}
                      className="min-h-11"
                      {...autoComplete('name')}
                    />
                  ) : null}
                  <TextInput
                    label="Password"
                    type="password"
                    value={password}
                    onChange={(v) => { setPassword(v); setIssue(null); }}
                    htmlName="password"
                    placeholder={isSignup ? 'At least 8 characters' : 'Your password'}
                    isRequired
                    status={fieldStatus('password')}
                    className="min-h-11"
                    {...autoComplete(isSignup ? 'new-password' : 'current-password')}
                  />
                  {isSignup ? (
                    <TextInput
                      label="Workspace name"
                      value={workspaceName}
                      onChange={(v) => { setWorkspaceName(v); setIssue(null); }}
                      htmlName="workspaceName"
                      placeholder="Acme Inc."
                      isRequired
                      status={fieldStatus('workspaceName')}
                      className="min-h-11"
                      {...autoComplete('organization')}
                    />
                  ) : null}
                  <Button
                    type="submit"
                    variant="primary"
                    size="lg"
                    label={
                      loading
                        ? isSignup ? 'Creating workspace…' : 'Signing in…'
                        : isSignup ? 'Create workspace' : 'Log in'
                    }
                    isLoading={loading}
                    className="w-full min-h-11"
                  />
                </VStack>
              </form>

              {/* A real Button, not a tablist. The old ModeToggle declared
                  role="tablist"/"tab" with no panel and no arrow keys — a screen
                  reader announced "tab 1 of 2" and found nothing. SegmentedControl
                  is not the fix either: its selected thumb uses
                  --color-background-surface, which reads recessed on this dark
                  card. Keep this a ghost Button. */}
              <VStack gap={1} align="center">
                <Text type="supporting">
                  {isSignup ? 'Already have an account?' : 'No account yet?'}
                </Text>
                <Button
                  variant="ghost"
                  size="lg"
                  label={isSignup ? 'Log in' : 'Create one'}
                  onClick={() => switchMode(isSignup ? 'login' : 'signup')}
                  className="min-h-11"
                />
              </VStack>
            </VStack>
          </Card>
        </VStack>
      </Center>
    </main>
  );
}
