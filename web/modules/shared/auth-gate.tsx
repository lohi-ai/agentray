'use client';

import { useState } from 'react';
import { Waypoints } from 'lucide-react';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { Text } from '@astryxdesign/core/Text';
import { apiBase } from '@/lib/api';
import { formatAuthError } from '@/lib/auth-form';
import { useAuth } from '@/modules/app/hooks';
import { AuthScreen } from '@/modules/shared/auth-screen';

// AuthGate runs the session check once and decides what to render: something
// while `me()` is in flight, the AuthScreen when there is no session, and the
// app itself once a workspace session is present. Every screen mounts inside
// it, so pages can assume an authenticated project is available.
//
// What renders during the unresolved state is a *crawlability* decision, not a
// cosmetic one. `authChecked` starts false (lib/app-state.ts), so this branch is
// what the server emits for every URL — and when it was a centred "Checking your
// workspace session…" splash, the only indexable page on the site was a spinner.
// Every meta description, OG card and JSON-LD string was pointing at a page with
// no product copy on it.
//
// So: no session cookie on the request → render the door. The pitch becomes the
// server-rendered content of `/`, and a signed-out visitor stops seeing a
// spinner that jumps to a form. `hasSession` is read server-side in app/layout.tsx
// from the `agentray_session` cookie, so a returning user still gets the quiet
// splash instead of a flash of the door they do not need.
export function AuthGate({ children, hasSession = false }: { children: React.ReactNode; hasSession?: boolean }) {
  const { auth, authChecked, loading, submitAuth } = useAuth();
  const [error, setError] = useState('');

  const door = (
    <AuthScreen
      loading={loading}
      pending={!authChecked}
      error={error}
      onModeChange={() => setError('')}
      onSubmit={async (input) => {
        setError('');
        try {
          await submitAuth(input);
        } catch (err) {
          setError(formatAuthError(err, apiBase()));
        }
      }}
    />
  );

  // A returning user: the cookie says a session exists, so the door would be a
  // flash of the wrong screen. Wait quietly instead. `motion-reduce` kills the
  // pulse for anyone who asked the OS for less motion — an indefinite loop is
  // exactly the animation that setting exists for.
  if (!authChecked && hasSession) {
    return (
      <div className="flex min-h-dvh flex-col items-center justify-center gap-4">
        <span className="[animation:pulse_2s_var(--ease)_infinite] motion-reduce:[animation:none]">
          <NavIcon icon={<Waypoints size={18} />} />
        </span>
        <Text type="supporting">Checking your workspace session…</Text>
      </div>
    );
  }

  // Same element in both branches on purpose: React keeps AuthScreen mounted
  // across the unresolved → signed-out transition, so anything already typed
  // survives `me()` answering.
  if (!authChecked || !auth) return door;

  return <>{children}</>;
}
