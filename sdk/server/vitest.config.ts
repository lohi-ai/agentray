import { defineConfig } from 'vitest/config';

// node, not jsdom: this client exists precisely because it runs where there is
// no browser, and nothing it does touches the DOM.
export default defineConfig({
  test: { environment: 'node', include: ['test/**/*.test.ts'], restoreMocks: true },
});
