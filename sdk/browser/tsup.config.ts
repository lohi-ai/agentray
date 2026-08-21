import { defineConfig } from 'tsup';

// Three outputs, because a web SDK is installed three ways and the third one is
// the most common:
//   esm + cjs — bundler consumers (`npm i @agentray/browser`)
//   iife      — a plain <script> tag, which is how a marketing site, a Framer
//               page, or a Webflow project adds analytics. Exposed as the global
//               `AgentRay`, so the copy-paste snippet the product hands out is
//               this same tested bundle rather than a second implementation of
//               the contract that has to be kept in sync by hand.
// The bundle is dependency-free (browser globals only), so no externals.
export default defineConfig({
  entry: ['index.ts'],
  format: ['esm', 'cjs', 'iife'],
  globalName: 'AgentRay',
  dts: true,
  clean: true,
  minify: true,
  sourcemap: true,
  target: 'es2018',
});
