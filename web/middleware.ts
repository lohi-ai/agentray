import { NextResponse, type NextRequest } from 'next/server';
import { SESSION_COOKIE } from '@/lib/session-cookie';

// AuthGate renders the door for a signed-out visitor — but it renders it
// in place, so /overview answered the landing page under the /overview URL.
// A stranger following a stale link should land on the pitch, not on a copy
// of it wearing the wrong address: redirect every gated route to `/` when
// the session cookie is absent. `/` itself is the one public URL (robots.ts
// disallows everything else for the same reason).
//
// Production only: in dev the API sits on another origin and the host-only
// session cookie never reaches the web app (app/layout.tsx documents dev
// always taking the stranger path), so gating on the cookie here would
// redirect every route to `/` and make the app unreachable.
export function middleware(request: NextRequest) {
  if (process.env.NODE_ENV !== 'production') return NextResponse.next();
  if (request.cookies.has(SESSION_COOKIE)) return NextResponse.next();
  const url = request.nextUrl.clone();
  url.pathname = '/';
  url.search = '';
  return NextResponse.redirect(url);
}

export const config = {
  // Everything except `/`, Next internals, and file assets.
  matcher: ['/((?!$|_next/|api/|.*\\..*).*)'],
};
