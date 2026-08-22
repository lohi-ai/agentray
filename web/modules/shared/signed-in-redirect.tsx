'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { signedInLandingTarget } from '@/lib/ia';
import { useAuth } from '@/modules/app/hooks';

// The client half of `/`'s landing decision. app/page.tsx redirects server-side
// when the request carries a session cookie; where it cannot see one — a
// split-origin deployment, or local dev — this picks the same target up once
// `me()` has answered. It renders nothing: AuthGate is showing either the door
// or the splash the whole time it is mounted.
export function SignedInRedirect() {
  const { auth, authChecked } = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (!authChecked || !auth) return;
    router.replace(signedInLandingTarget());
  }, [auth, authChecked, router]);

  return null;
}
