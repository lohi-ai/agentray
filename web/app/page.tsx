import { redirect } from 'next/navigation';
import { cookies } from 'next/headers';
import { signedInLandingTarget } from '@/lib/ia';
import { SESSION_COOKIE } from '@/modules/shared/session-cookie';
import { SignedInRedirect } from '@/modules/shared/signed-in-redirect';

// Conversation is the front door for a signed-in session. Saved views stay at
// /dashboard; a signed-in session should land in chat so the first action is
// asking, not scanning a board.
//
// This used to redirect unconditionally, which meant `/` — the one URL anybody
// links to or crawls — answered 307 to a gated route for everyone, including a
// stranger with no session. The pitch had no URL at all. Now the redirect is
// conditional on the session cookie: a visitor without one gets `/` itself,
// where AuthGate (app/layout.tsx) renders the door as server HTML.
//
// `children` is `null` for that visitor: AuthGate never renders children when
// there is no session, so there is nothing for this page to contribute.
export default async function Home() {
  const hasSession = (await cookies()).has(SESSION_COOKIE);
  if (hasSession) redirect(signedInLandingTarget());

  // The cookie is host-only, so anywhere web and the API are on different
  // origins (local dev: :3200 vs :8088) a signed-in user reaches here too. The
  // client knows better once `me()` resolves — this hop is what keeps that user
  // landing in chat instead of staring at an empty page.
  return <SignedInRedirect />;
}
