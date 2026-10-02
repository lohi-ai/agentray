import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

const source = (path: string) => fileURLToPath(new URL(`./.work/packages/${path}`, import.meta.url));

export default defineConfig({
  test: {
    include: [".work/packages/{agent,telemetry}/test/**/*.test.ts"],
    environment: "node",
    testTimeout: 30000,
  },
  resolve: {
    alias: [
      { find: /^@earendil-works\/pi-telemetry$/, replacement: source("telemetry/src/index.ts") },
      { find: /^@earendil-works\/pi-agent-core$/, replacement: source("agent/src/index.ts") },
      { find: /^@earendil-works\/pi-ai\/compat$/, replacement: source("ai/src/compat.ts") },
      { find: /^@earendil-works\/pi-ai$/, replacement: source("ai/src/index.ts") },
    ],
  },
});
