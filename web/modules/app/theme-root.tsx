'use client';

import { Theme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';
import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';

export type ColorMode = 'light' | 'dark';

type ColorModeContextValue = {
  mode: ColorMode;
  setMode: (mode: ColorMode) => void;
  toggle: () => void;
};

const ColorModeContext = createContext<ColorModeContextValue | null>(null);

export function useColorMode(): ColorModeContextValue {
  const ctx = useContext(ColorModeContext);
  if (!ctx) throw new Error('useColorMode must be used within <ThemeRoot>');
  return ctx;
}

const STORAGE_KEY = 'agentray-color-mode';

/**
 * App-wide Astryx Theme provider. We use the pre-built (`/built`) neutral theme
 * + its compiled `theme.css` (imported in globals.css) so component overrides are
 * present during SSR with no hydration flash — the supported path for Next.js.
 *
 * Dark is AgentRay's historical identity, so it's the deterministic first-paint
 * value (SSR and client agree); any stored user choice is adopted after mount.
 */
export function ThemeRoot({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ColorMode>('dark');

  // Read the stored choice after mount (not in a lazy initializer) so the server
  // and first client render both produce 'dark' and never mismatch on hydration.
  useEffect(() => {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    // eslint-disable-next-line react-hooks/set-state-in-effect -- intentional post-hydration adoption of persisted mode
    if (stored === 'light' || stored === 'dark') setModeState(stored);
  }, []);

  const setMode = (next: ColorMode) => {
    setModeState(next);
    try {
      window.localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // Storage may be unavailable (private mode); mode still applies in-session.
    }
  };

  return (
    <ColorModeContext.Provider
      value={{ mode, setMode, toggle: () => setMode(mode === 'dark' ? 'light' : 'dark') }}
    >
      <Theme theme={neutralTheme} mode={mode}>
        {children}
      </Theme>
    </ColorModeContext.Provider>
  );
}
