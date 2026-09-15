import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  output: 'standalone',
  // /prototypes is the pre-Plans name for the same validation_tests list. A
  // config redirect runs before rendering, so it emits a real 308 — a
  // permanentRedirect() in page.tsx can't, because the root layout's
  // await cookies() has already started streaming by the time the page runs.
  // modules/prototypes stays on disk as the design artifact /plans reads from.
  // The legacy analytics surfaces retired into the declared boards (006):
  // /traffic and /web-analytics answered acquisition questions, /product
  // answered usage questions. Permanent redirects keep the deep links.
  async redirects() {
    return [
      { source: '/prototypes', destination: '/plans', permanent: true },
      { source: '/prototypes/:prototypeId', destination: '/plans/:prototypeId', permanent: true },
      { source: '/traffic', destination: '/acquisition', permanent: true },
      { source: '/web-analytics', destination: '/acquisition', permanent: true },
      { source: '/product', destination: '/usage', permanent: true },
    ];
  },
};

export default nextConfig;
