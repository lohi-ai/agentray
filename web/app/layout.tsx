import type { Metadata } from 'next';
import { cookies } from 'next/headers';
import { Inter, Geist_Mono } from 'next/font/google';
import { NuqsAdapter } from 'nuqs/adapters/next/app';
import { Toaster } from 'sonner';
// Cascade-layer order must be declared before any layered CSS loads.
import './cascade-layers.css';
// Astryx vendored CSS imported here (not via @import in globals.css) because
// Turbopack drops bare node_modules @imports — see the note in globals.css.
import '@astryxdesign/core/reset.css';
import '@astryxdesign/core/astryx.css';
import '@astryxdesign/theme-neutral/theme.css';
import '@astryxdesign/core/tailwind-theme.css';
import './globals.css';
import {
  BRAND_NAME,
  META_DESCRIPTION,
  siteOrigin,
  softwareApplicationJsonLd,
  TITLE_DEFAULT,
  TITLE_TEMPLATE,
} from '@/lib/brand';
import { AppProvider } from '@/modules/app/providers';
import { ThemeRoot } from '@/modules/app/theme-root';
import { AuthGate } from '@/modules/shared/auth-gate';
import { StackSheetProvider } from '@/modules/shared/components/stack-sheet';
import { SESSION_COOKIE } from '@/modules/shared/session-cookie';

const inter = Inter({ subsets: ['latin', 'vietnamese'], variable: '--font-inter', display: 'swap' });
const geistMono = Geist_Mono({ subsets: ['latin'], variable: '--font-geist-mono', display: 'swap' });

// Every string here comes from lib/brand.ts, so the tab title, the search
// snippet and the shared card cannot drift from what the door says. `%s ·
// AgentRay` keeps the brand as a suffix; a deep page that sets its own title
// still leads with its own subject.
//
// metadataBase is required for OpenGraph/canonical to resolve to absolute URLs;
// without it Next silently emits relative ones and the OG card breaks on every
// consumer. See `siteOrigin()` for where the origin comes from on a self-hosted
// instance.
export const metadata: Metadata = {
  metadataBase: new URL(siteOrigin()),
  title: { default: TITLE_DEFAULT, template: TITLE_TEMPLATE },
  description: META_DESCRIPTION,
  applicationName: BRAND_NAME,
  alternates: { canonical: '/' },
  openGraph: {
    type: 'website',
    siteName: BRAND_NAME,
    title: TITLE_DEFAULT,
    description: META_DESCRIPTION,
    url: '/',
  },
  twitter: {
    card: 'summary_large_image',
    title: TITLE_DEFAULT,
    description: META_DESCRIPTION,
  },
  // Only `/` has content; app/robots.ts disallows the gated routes, and this
  // says the same thing to a crawler that reached one anyway.
  robots: { index: true, follow: true },
};

export default async function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  // Reading a cookie makes every route dynamic. That is already true in
  // practice — nothing here is statically useful, every screen is behind
  // AuthGate — and it is what lets the server tell a returning user (quiet
  // splash) from a stranger (the door, which is the only page a crawler can
  // index). Locally web is :3200 and the API is :8088, and `agentray_session`
  // is host-only (no Domain, internal/app/auth.go), so dev always takes the
  // stranger path. That is expected; do not "fix" it by widening the cookie.
  const hasSession = (await cookies()).has(SESSION_COOKIE);

  return (
    <html lang="en" className={`${inter.variable} ${geistMono.variable}`}>
      <body>
        <script
          type="application/ld+json"
          // Static, hand-built object — no user input reaches it.
          dangerouslySetInnerHTML={{ __html: JSON.stringify(softwareApplicationJsonLd(siteOrigin())) }}
        />
        <ThemeRoot>
          <NuqsAdapter>
            <AppProvider>
              <StackSheetProvider>
                <AuthGate hasSession={hasSession}>{children}</AuthGate>
              </StackSheetProvider>
            </AppProvider>
          </NuqsAdapter>
        </ThemeRoot>
        <Toaster position="bottom-right" richColors />
      </body>
    </html>
  );
}
