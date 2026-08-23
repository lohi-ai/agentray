'use client';

import { useRef, useState } from 'react';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { TextInput, type TextInputProps } from '@astryxdesign/core/TextInput';
import { validateAuthForm, type AuthField, type AuthMode } from '@/lib/auth-form';
import { Landing } from '@/modules/landing/landing';

// Signup does not ask for a project name: naming a container is not a decision a
// stranger can make before seeing anything, and it is the one field here that is
// renameable later from the chrome. The constant is still sent so
// validateAuthForm's projectName rule passes and POST /api/auth/signup is
// unchanged.
const HIDDEN_PROJECT_NAME = 'Production';

// TextInput spreads unknown props straight onto the <input> (TextInput.js:146),
// but `autoComplete` is missing from TextInputProps because BaseProps extends
// React.HTMLAttributes, not InputHTMLAttributes. Without it a password manager
// offers the *existing* password on the signup form instead of generating one,
// so pass it through a narrow typed spread rather than dropping it.
function autoComplete(value: string): Partial<TextInputProps> {
  return { autoComplete: value } as Partial<TextInputProps>;
}

// AuthScreen still owns every piece of form behaviour it has always owned —
// mode, field values, validation, the API error banner, the pending gate. What
// changed is what surrounds it: the door used to be a two-column card centred
// in the viewport, which meant `/`, the only URL a crawler can index, was a
// login box with a paragraph beside it. The pitch is now a real landing page
// (modules/landing/), and this component supplies it with the form and the mode
// it should be in. Nothing about the form itself moved.
export function AuthScreen({
  loading,
  pending = false,
  error,
  onSubmit,
  onModeChange,
}: {
  loading: boolean;
  // The session check has not answered yet. The door still renders — it is the
  // only crawlable content this app has, and a spinner in its place is what
  // made every SEO string on the site decorative (see auth-gate.tsx). Only
  // submitting is held back, so nobody can start a signup that races `me()`.
  pending?: boolean;
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
  const [showPassword, setShowPassword] = useState(false);
  const emailRef = useRef<HTMLInputElement>(null);
  const isSignup = mode === 'signup';

  // The form no longer sits above the fold, so autofocusing it would scroll a
  // first-time visitor past the entire pitch before they have read a word. It
  // is focused when the reader asks for it instead — switchMode is called by
  // both landing CTAs — and only where a keyboard is already there. On a touch
  // device the popped keyboard halves an already-tall card.
  function focusForm() {
    if (typeof window === 'undefined') return;
    if (pending) return;
    if (window.matchMedia?.('(pointer: fine)').matches) emailRef.current?.focus();
  }

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

  // Reached from the landing bar and the hero CTA: pick the mode, then put the
  // caret in the first field once the scroll has landed. Focusing *before* the
  // scroll settles would make the browser scroll the input into view itself and
  // fight the smooth scroll the CTA just started, so the wait is deliberate —
  // and skipped entirely when the reader has asked for less motion, because
  // then the scroll was instant.
  function pickMode(next: AuthMode) {
    if (next !== mode) switchMode(next);
    const instant = typeof window !== 'undefined'
      && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
    if (instant) focusForm();
    else window.setTimeout(focusForm, 450);
  }

  const form = (
    <Card padding={5} width="100%" maxWidth={420}>
      <VStack gap={4} align="stretch">
        <VStack gap={1} align="start">
          <Heading level={3} className="text-lg">{isSignup ? 'Create your workspace' : 'Welcome back'}</Heading>
          <Text type="supporting">
            {isSignup
              ? 'Set up in a minute — every name here is editable later.'
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
            {/* Show/hide is a plain toggle button rather than an input adornment:
                Astryx TextInput has no trailing-slot prop, and a manager-filled
                password a reader cannot check is the single most common cause of
                a failed first sign-in. */}
            <VStack gap={1} align="stretch">
              <TextInput
                label="Password"
                type={showPassword ? 'text' : 'password'}
                value={password}
                onChange={(v) => { setPassword(v); setIssue(null); }}
                htmlName="password"
                placeholder={isSignup ? 'At least 8 characters' : 'Your password'}
                isRequired
                status={fieldStatus('password')}
                className="min-h-11"
                {...autoComplete(isSignup ? 'new-password' : 'current-password')}
              />
              <Button
                variant="ghost"
                size="sm"
                label={showPassword ? 'Hide password' : 'Show password'}
                onClick={() => setShowPassword((v) => !v)}
                className="self-start"
              />
            </VStack>
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
              isDisabled={pending}
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
  );

  return <Landing mode={mode} onPickMode={pickMode} form={form} />;
}
