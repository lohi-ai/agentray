'use client';

// PROTOTYPE — bs-hb8tk1j1. Throwaway. implement rebuilds from ticket design.md.
// Not linked from production navigation.
// Open (after sign-in): http://localhost:3200/prototype/auth

import { useState } from 'react';
import { Waypoints } from 'lucide-react';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { validateAuthForm, type AuthField, type AuthMode } from '@/lib/auth-form';

type Fixture = 'signup' | 'login' | 'field-error' | 'api-error' | 'splash';

export default function AuthPrototypePage() {
  const [fixture, setFixture] = useState<Fixture>('signup');

  return (
    <div className="min-h-dvh bg-[var(--color-background-body)]">
      <div className="flex flex-wrap gap-2 border-b border-[var(--color-border)] px-4 py-3">
        <Text type="supporting">Prototype fixtures — not production chrome</Text>
        {(['signup', 'login', 'field-error', 'api-error', 'splash'] as const).map((id) => (
          <Button
            key={id}
            size="sm"
            variant={fixture === id ? 'primary' : 'secondary'}
            label={id}
            onClick={() => setFixture(id)}
          />
        ))}
      </div>
      {fixture === 'splash' ? <SessionSplash /> : <AuthCard key={fixture} fixture={fixture} />}
    </div>
  );
}

function SessionSplash() {
  return (
    <Center axis="both" height="80dvh">
      <VStack gap={3} align="center">
        <span className="[animation:pulse_2s_var(--ease)_infinite]">
          <NavIcon icon={<Waypoints size={18} />} />
        </span>
        <Text type="supporting">Checking your workspace session…</Text>
      </VStack>
    </Center>
  );
}

function AuthCard({ fixture }: { fixture: Exclude<Fixture, 'splash'> }) {
  const presetMode: AuthMode = fixture === 'login' ? 'login' : 'signup';
  const [mode, setMode] = useState<AuthMode>(presetMode);
  const [email, setEmail] = useState(fixture === 'field-error' ? 'not-an-email' : '');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [workspaceName, setWorkspaceName] = useState('My workspace');
  const [projectName, setProjectName] = useState('Production');
  const [issue, setIssue] = useState<{ field: AuthField; message: string } | null>(
    fixture === 'field-error'
      ? { field: 'email', message: 'Enter a valid email address.' }
      : null,
  );
  const [error] = useState(fixture === 'api-error'
    ? 'Can’t reach AgentRay at http://localhost:8088. Start the API and try again.'
    : '');
  const [loading, setLoading] = useState(false);

  const isSignup = mode === 'signup';

  function fieldStatus(field: AuthField) {
    return issue?.field === field ? { type: 'error' as const, message: issue.message } : undefined;
  }

  function switchMode(next: AuthMode) {
    setMode(next);
    setIssue(null);
  }

  return (
    <Center axis="both" height="80dvh">
      <VStack gap={4} align="center" className="w-full max-w-[400px] px-5">
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
                {isSignup ? 'Then ask what changed.' : 'Sign in to keep going.'}
              </Text>
            </VStack>

            {error ? <Banner status="error" title={error} /> : null}

            <form
              onSubmit={(event) => {
                event.preventDefault();
                const next = validateAuthForm({
                  mode, email, name, password, workspaceName, projectName,
                });
                setIssue(next);
                if (next) return;
                setLoading(true);
                window.setTimeout(() => setLoading(false), 800);
              }}
            >
              <VStack gap={3} align="stretch">
                <TextInput
                  label="Email"
                  type="email"
                  value={email}
                  onChange={(v) => { setEmail(v); setIssue(null); }}
                  htmlName="email"
                  placeholder="you@company.com"
                  isRequired
                  status={fieldStatus('email')}
                  hasAutoFocus
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
                />
                {isSignup ? (
                  <>
                    <TextInput
                      label="Workspace name"
                      value={workspaceName}
                      onChange={(v) => { setWorkspaceName(v); setIssue(null); }}
                      htmlName="workspaceName"
                      placeholder="Acme Inc."
                      isRequired
                      status={fieldStatus('workspaceName')}
                    />
                    <TextInput
                      label="Project name"
                      value={projectName}
                      onChange={(v) => { setProjectName(v); setIssue(null); }}
                      htmlName="projectName"
                      placeholder="Production"
                      isRequired
                      status={fieldStatus('projectName')}
                    />
                  </>
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
                  className="w-full"
                />
              </VStack>
            </form>

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
  );
}
