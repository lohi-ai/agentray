import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  output: 'standalone',
  // /prototypes is the pre-Plans name for the same validation_tests list. A
  // config redirect runs before rendering, so it emits a real 308 — a
  // permanentRedirect() in page.tsx can't, because the root layout's
  // await cookies() has already started streaming by the time the page runs.
  // modules/prototypes stays on disk as the design artifact /plans reads from.
  async redirects() {
    return [
      { source: '/prototypes', destination: '/plans', permanent: true },
      { source: '/prototypes/:prototypeId', destination: '/plans/:prototypeId', permanent: true },
    ];
  },
};

export default nextConfig;
