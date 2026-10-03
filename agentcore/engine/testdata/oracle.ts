// Load the pinned implementation without depending on the reference catalog.
import { plugin } from "bun";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

export const root = new URL("../../../third_party/pi/", import.meta.url);
execFileSync("python3", [new URL("verify.py", root).pathname], { stdio: "inherit" });
export const aiRoot = new URL("upstream/packages/ai/src/", root);
export const manifest = JSON.parse(readFileSync(new URL("UPSTREAM.json", root), "utf8"));
plugin({ name: "pi-agent-oracle", setup(build) {
  build.onResolve({ filter: /^@earendil-works\/pi-ai$/ }, () => ({ path: "pi-ai", namespace: "oracle" }));
  build.onLoad({ filter: /.*/, namespace: "oracle" }, () => ({ loader: "js", contents:
    `export * from ${JSON.stringify(new URL("utils/transcript.ts", aiRoot).pathname)};
     export * from ${JSON.stringify(new URL("utils/event-stream.ts", aiRoot).pathname)};
     export * from ${JSON.stringify(new URL("utils/validation.ts", aiRoot).pathname)};
     export * from ${JSON.stringify(new URL("utils/json-parse.ts", aiRoot).pathname)};`
  }));
}});
