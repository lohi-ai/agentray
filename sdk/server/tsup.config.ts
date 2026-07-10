import { defineConfig } from 'tsup';

// Dual ESM + CJS build with type declarations, matching @agentray/browser.
// Dependency-free (global fetch, Node >= 18), so no externals.
export default defineConfig({
  entry: ['index.ts'],
  format: ['esm', 'cjs'],
  dts: true,
  clean: true,
  minify: true,
  sourcemap: true,
  target: 'es2020',
});
