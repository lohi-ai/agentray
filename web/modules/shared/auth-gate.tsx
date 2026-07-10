'use client';

import { Waypoints } from 'lucide-react';
import { NavIcon } from '@astryxdesign/core/NavIcon';
import { Text } from '@astryxdesign/core/Text';
import { useAuth } from '@/modules/app/hooks';
import { AuthScreen } from '@/modules/shared/auth-screen';

// AuthGate runs the session check once and decides what to render: a loading
// splash while `me()` is in flight, the AuthScreen when there is no session, and
// the app itself once a workspace session is present. Every screen mounts inside
// it, so pages can assume an authenticated project is available.
export function AuthGate({ children }: { children: React.ReactNode }) {
  const { auth, authChecked, loading, submitAuth } = useAuth();

  if (!authChecked) {
    return (
      <div className="flex min-h-dvh flex-col items-center justify-center gap-3.5">
        <span className="[animation:pulse_2s_var(--ease)_infinite]">
          <NavIcon icon={<Waypoints size={18} />} />
        </span>
        <Text type="supporting">Checking your workspace session…</Text>
      </div>
    );
  }

  if (!auth) {
    return <AuthScreen loading={loading} error="" onSubmit={submitAuth} />;
  }

  return <>{children}</>;
}
