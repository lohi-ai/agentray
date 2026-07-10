import { defineConfig } from 'tsup';

// Dual ESM + CJS build with type declarations, the shape npm consumers expect.
// The bundle is dependency-free (browser globals only), so no externals.
export default defineConfig({
  entry: ['index.ts'],
  format: ['esm', 'cjs'],
  dts: true,
  clean: true,
  minify: true,
  sourcemap: true,
  target: 'es2018',
});
