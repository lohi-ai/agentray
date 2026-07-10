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
  onSubmit: (input: { mode: 'login' | 'signup'; email: string; name: string; password: string; workspaceName: string; projectName: string }) => Promise<void>;
}) {
  const [mode, setMode] = useState<'login' | 'signup'>('login');
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [workspaceName, setWorkspaceName] = useState('');
  const [projectName, setProjectName] = useState('');
  const isSignup = mode === 'signup';

  return (
    <main
      className="relative min-h-screen overflow-hidden px-5 py-8 sm:px-8"
      style={{ background: PAGE_BACKGROUND, color: 'var(--color-text-primary)' }}
    >
      <div className="mx-auto grid min-h-[calc(100dvh-4rem)] w-full max-w-[1080px] items-center gap-10 lg:grid-cols-[minmax(0,1fr)_420px] lg:gap-16">
        {/* Brand / value proposition */}
        <section className="order-2 lg:order-1">
          <VStack gap={6} align="start">
            <BrandMark />
            <VStack gap={3} align="start">
              <Badge variant="green" label="Growth operating system with agents" />
              <Heading level={1}>Turn product signals into agent-assisted growth moves.</Heading>
              <Text type="body" color="secondary">
                Ask what changed in plain language, see what moved across traffic and product, and let
                AI agents investigate and recommend the next step — all from one workspace.
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
            <Text type="supporting">Open-source · Self-hostable · MCP-ready</Text>
          </VStack>
        </section>

        {/* Auth card */}
        <div className="order-1 lg:order-2">
          <Card
            padding={5}
            className="shadow-[0_30px_80px_-28px_rgba(0,0,0,0.6)] backdrop-blur-sm"
          >
            <VStack gap={4} align="stretch">
              <ModeToggle mode={mode} onChange={setMode} />

              <VStack gap={1} align="start">
                <Heading level={3}>{isSignup ? 'Create your workspace' : 'Welcome back'}</Heading>
                <Text type="supporting">
                  {isSignup
                    ? 'A workspace holds your projects and event streams. Set it up in seconds.'
                    : 'Sign in to your AgentRay dashboard.'}
                </Text>
              </VStack>

              {error ? <Banner status="error" title={error} /> : null}

              <form
                onSubmit={(event) => {
                  event.preventDefault();
                  void onSubmit({ mode, email, name, password, workspaceName, projectName });
                }}
              >
                <VStack gap={3} align="stretch">
                  <TextInput label="Email" type="email" value={email} onChange={setEmail} htmlName="email" placeholder="you@company.com" />
                  {isSignup ? (
                    <TextInput label="Full name" value={name} onChange={setName} htmlName="name" placeholder="Ada Lovelace" />
                  ) : null}
                  <TextInput label="Password" type="password" value={password} onChange={setPassword} htmlName="password" placeholder={isSignup ? 'At least 8 characters' : '••••••••'} />
                  {isSignup ? (
                    <>
                      <TextInput label="Workspace name" value={workspaceName} onChange={setWorkspaceName} htmlName="workspaceName" placeholder="Acme Inc." />
                      <TextInput label="Project name" value={projectName} onChange={setProjectName} htmlName="projectName" placeholder="Production" />
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
function ModeToggle({ mode, onChange }: { mode: 'login' | 'signup'; onChange: (m: 'login' | 'signup') => void }) {
  const tabs: { value: 'login' | 'signup'; label: string }[] = [
    { value: 'login', label: 'Log in' },
    { value: 'signup', label: 'Sign up' },
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
            className={`h-9 rounded-[var(--radius-md)] text-[13.5px] font-medium transition-colors ${
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
