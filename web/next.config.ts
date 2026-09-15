import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  output: 'standalone',
  // The legacy analytics surfaces retired into the declared boards (006):
  // /traffic and /web-analytics answered acquisition questions, /product
  // answered usage questions. Permanent redirects keep the deep links.
  async redirects() {
    return [
      { source: '/traffic', destination: '/acquisition', permanent: true },
      { source: '/web-analytics', destination: '/acquisition', permanent: true },
      { source: '/product', destination: '/usage', permanent: true },
    ];
  },
};

export default nextConfig;
