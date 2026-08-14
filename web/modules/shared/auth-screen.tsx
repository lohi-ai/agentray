'use client';

import { useState } from 'react';
import { Activity, MessageSquareText, Sparkles, Waypoints } from 'lucide-react';
import type { ComponentType, SVGProps } from 'react';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { VStack } from '@astryxdesign/core/Stack';
import { Heading, Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { validateAuthForm, type AuthField, type AuthMode } from '@/lib/auth-form';

type Feature = { icon: ComponentType<SVGProps<SVGSVGElement>>; label: string; detail: string };

const FEATURES: Feature[] = [
  { icon: MessageSquareText, label: 'Ask', detail: 'Question funnels, retention, and traffic without writing SQL.' },
  { icon: Activity, label: 'Notice', detail: 'See what moved across product and acquisition at a glance.' },
  { icon: Sparkles, label: 'Act', detail: 'Agents investigate and recommend the safest next move.' },
];

// Two soft brand glows (agent purple, primary green) over the themed page surface.
// Brand tokens are mode-constant, so the glow reads in both light and dark while
// --color-background-body flips — keeping the screen on-brand without hardcoded hex.
const PAGE_BACKGROUND =
  'radial-gradient(58rem 40rem at 12% 8%, color-mix(in srgb, var(--agent) 16%, transparent), transparent 58%),' +
  'radial-gradient(52rem 40rem at 96% 96%, color-mix(in srgb, var(--primary) 13%, transparent), transparent 55%),' +
  'var(--color-background-body)';

export function AuthScreen({
  loading,
  error,
  onSubmit,
}: {
  loading: boolean;
  error: string;
  onSubmit: (input: { mode: AuthMode; email: string; name: string; password: string; workspaceName: string; projectName: string }) => Promise<void>;
}) {
  // Sign up first: a stranger's job is to create a workspace, not guess they already have one.
  const [mode, setMode] = useState<AuthMode>('signup');
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [workspaceName, setWorkspaceName] = useState('My workspace');
  const [projectName, setProjectName] = useState('Production');
  const [issue, setIssue] = useState<{ field: AuthField; message: string } | null>(null);
  const isSignup = mode === 'signup';

  function fieldStatus(field: AuthField) {
    return issue?.field === field ? { type: 'error' as const, message: issue.message } : undefined;
  }

  function switchMode(next: AuthMode) {
    setMode(next);
    setIssue(null);
  }

  return (
    <main
      className="relative flex min-h-dvh items-center px-5 py-8 sm:px-8"
      style={{ background: PAGE_BACKGROUND, color: 'var(--color-text-primary)' }}
    >
      <div className="mx-auto grid w-full max-w-[1080px] items-center gap-10 lg:grid-cols-[minmax(0,1fr)_420px] lg:gap-16">
        {/* Brand / value proposition */}
        <section className="order-2 lg:order-1">
          <VStack gap={6} align="start">
            <BrandMark />
            <VStack gap={3} align="start">
              <Badge variant="green" label="Growth operating system with agents" />
              <Heading level={1}>Turn product signals into agent-assisted growth moves.</Heading>
              <Text type="body" color="secondary">
                Sign up and land in a live Demo workspace — a real funnel, a written weakest step —
                then connect your website, app, or warehouse to replace the sample with your data.
              </Text>
            </VStack>
            <div className="grid w-full gap-1">
              {FEATURES.map(({ icon: Icon, label, detail }) => (
                <div key={label} className="flex items-start gap-3 rounded-[var(--radius-lg)] px-2 py-2.5 transition-colors hover:bg-[var(--color-background-card)]">
                  <span
                    className="mt-px grid size-9 flex-none place-items-center rounded-[var(--radius-md)]"
                    style={{ background: 'color-mix(in srgb, var(--primary) 14%, transparent)', color: 'var(--primary)' }}
                    aria-hidden
                  >
                    <Icon width={17} height={17} />
                  </span>
                  <div className="flex min-w-0 flex-col gap-0.5">
                    <Text type="body" weight="medium">{label}</Text>
                    <Text type="supporting">{detail}</Text>
                  </div>
                </div>
              ))}
            </div>
            <Text type="supporting">Ask · Notice · Act · Open-source · Self-hostable</Text>
          </VStack>
        </section>

        {/* Auth card */}
        <div className="order-1 lg:order-2">
          <Card
            padding={5}
            className="shadow-[0_30px_80px_-28px_rgba(0,0,0,0.6)] backdrop-blur-sm"
          >
            <VStack gap={4} align="stretch">
              <ModeToggle mode={mode} onChange={switchMode} />

              <VStack gap={1} align="start">
                <Heading level={3}>{isSignup ? 'Start asking in a minute' : 'Welcome back'}</Heading>
                <Text type="supporting">
                  {isSignup
                    ? 'Create a workspace, then ask what changed. No SQL setup first.'
                    : 'Sign in to start asking what changed.'}
                </Text>
              </VStack>

              {error ? <Banner status="error" title={error} /> : null}

              <form
                onSubmit={(event) => {
                  event.preventDefault();
                  const next = validateAuthForm({ mode, email, name, password, workspaceName, projectName });
                  setIssue(next);
                  if (next) return;
                  void onSubmit({ mode, email, name, password, workspaceName, projectName });
                }}
              >
                <VStack gap={3} align="stretch">
                  <TextInput label="Email" type="email" value={email} onChange={(v) => { setEmail(v); setIssue(null); }} htmlName="email" placeholder="you@company.com" isRequired status={fieldStatus('email')} hasAutoFocus />
                  {isSignup ? (
                    <TextInput label="Full name" value={name} onChange={(v) => { setName(v); setIssue(null); }} htmlName="name" placeholder="Ada Lovelace" isRequired status={fieldStatus('name')} />
                  ) : null}
                  <TextInput label="Password" type="password" value={password} onChange={(v) => { setPassword(v); setIssue(null); }} htmlName="password" placeholder={isSignup ? 'At least 8 characters' : 'Your password'} isRequired status={fieldStatus('password')} />
                  {isSignup ? (
                    <>
                      <TextInput label="Workspace name" value={workspaceName} onChange={(v) => { setWorkspaceName(v); setIssue(null); }} htmlName="workspaceName" placeholder="Acme Inc." isRequired status={fieldStatus('workspaceName')} />
                      <TextInput label="Project name" value={projectName} onChange={(v) => { setProjectName(v); setIssue(null); }} htmlName="projectName" placeholder="Production" isRequired status={fieldStatus('projectName')} />
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

              <Text type="supporting">
                {isSignup ? 'Already have an account? Switch to Log in above.' : 'No account yet? Switch to Sign up above.'}
              </Text>
            </VStack>
          </Card>
        </div>
      </div>
    </main>
  );
}

// Two-way mode toggle. Astryx's SegmentedControl selected thumb uses
// --color-background-surface, which in our dark theme is DARKER than the
// neutral-overlay track sitting on the card (card/muted/body all collapse to
// #1b1b1b while surface is #262626) — so its active state reads as recessed.
// Here the active pill uses --color-background-surface (the one neutral token
// that's lighter than the track in dark) plus elevation, so the selected tab
// reads as raised in BOTH light and dark.
function ModeToggle({ mode, onChange }: { mode: AuthMode; onChange: (m: AuthMode) => void }) {
  const tabs: { value: AuthMode; label: string }[] = [
    { value: 'signup', label: 'Sign up' },
    { value: 'login', label: 'Log in' },
  ];
  return (
    <div
      role="tablist"
      aria-label="Authentication mode"
      className="grid grid-cols-2 gap-1 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-background-muted)] p-1"
    >
      {tabs.map((t) => {
        const active = mode === t.value;
        return (
          <button
            key={t.value}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onChange(t.value)}
            className={`h-11 min-h-11 rounded-[var(--radius-md)] text-[13.5px] font-medium transition-colors ${
              active
                ? 'border border-[var(--color-border-emphasized)] bg-[var(--color-background-surface)] text-[var(--color-text-primary)] shadow-[var(--shadow-low)]'
                : 'border border-transparent text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]'
            }`}
          >
            {t.label}
          </button>
        );
      })}
    </div>
  );
}

function BrandMark() {
  return (
    <div className="flex items-center gap-3">
      <span
        className="grid size-10 flex-none place-items-center rounded-[var(--radius-lg)]"
        style={{ background: 'color-mix(in srgb, var(--primary) 16%, transparent)', color: 'var(--primary)' }}
        aria-hidden
      >
        <Waypoints size={20} />
      </span>
      <div className="leading-tight">
        <div className="text-[16px] font-semibold tracking-[-0.02em]">AgentRay</div>
        <div className="text-[12.5px] text-[var(--color-text-secondary)]">Growth · data · agents</div>
      </div>
    </div>
  );
}
