import { defineConfig } from 'vitest/config';

// jsdom, because every behaviour worth testing here is a browser behaviour:
// localStorage persistence, the unload beacon, delegated click capture. A node
// environment would pass while proving nothing.
export default defineConfig({
  test: {
    environment: 'jsdom',
    include: ['test/**/*.test.ts'],
    restoreMocks: true,
  },
});
