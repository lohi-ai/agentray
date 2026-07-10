import type { Metadata } from 'next';
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
import { AppProvider } from '@/modules/app/providers';
import { ThemeRoot } from '@/modules/app/theme-root';
import { AuthGate } from '@/modules/shared/auth-gate';
import { StackSheetProvider } from '@/modules/shared/components/stack-sheet';

const inter = Inter({ subsets: ['latin', 'vietnamese'], variable: '--font-inter', display: 'swap' });
const geistMono = Geist_Mono({ subsets: ['latin'], variable: '--font-geist-mono', display: 'swap' });

export const metadata: Metadata = {
  title: 'AgentRay Dashboard',
  description: 'Open-source analytics dashboard for AI-first products.',
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" className={`${inter.variable} ${geistMono.variable}`}>
      <body>
        <ThemeRoot>
          <NuqsAdapter>
            <AppProvider>
              <StackSheetProvider>
                <AuthGate>{children}</AuthGate>
              </StackSheetProvider>
            </AppProvider>
          </NuqsAdapter>
        </ThemeRoot>
        <Toaster position="bottom-right" richColors />
      </body>
    </html>
  );
}
