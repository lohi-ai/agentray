import { build } from "esbuild";
import { execFileSync } from "node:child_process";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
execFileSync("python3", ["verify.py"], { cwd: root, stdio: "inherit" });
await mkdir(`${root}dist`, { recursive: true });
const pin = JSON.parse(await readFile(`${root}UPSTREAM.json`, "utf8"));
const result = await build({
  absWorkingDir: root,
  tsconfig: "tsconfig.runtime.json",
  entryPoints: {
    agentcore: "upstream/packages/agent/src/index.ts",
    telemetry: "upstream/packages/telemetry/src/index.ts",
    "telemetry-testing": "upstream/packages/telemetry/src/testing/index.ts",
    worker: "runtime/main.ts",
    bridge: "runtime/worker.ts",
    stream: "runtime/native-stream.ts",
  },
  outdir: "dist",
  outExtension: { ".js": ".mjs" },
  bundle: true,
  splitting: true,
  format: "esm",
  platform: "node",
  target: "node22",
  metafile: true,
  sourcemap: true,
  define: { PI_UPSTREAM_COMMIT: JSON.stringify(pin.commit) },
  alias: {
    "@earendil-works/pi-agent-core": `${root}upstream/packages/agent/src/index.ts`,
    "@earendil-works/pi-ai": `${root}upstream/packages/ai/src/index.ts`,
    "@earendil-works/pi-telemetry": `${root}upstream/packages/telemetry/src/index.ts`,
  },
});
// Fail a build that accidentally resolves published or test-copy Pi code.
for (const input of Object.keys(result.metafile.inputs)) {
  if (input.includes(".work/") || input.includes("node_modules/@earendil-works/")) {
    throw new Error(`Build used an unverified Pi implementation: ${input}`);
  }
}
await writeFile(`${root}dist/build.json`, JSON.stringify({ commit: pin.commit, ...result.metafile }, null, 2) + "\n");
execFileSync("python3", ["verify.py"], { cwd: root, stdio: "inherit" });
