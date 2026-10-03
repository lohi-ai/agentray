// Development oracle: execute real throwing/reentrant getters in unchanged Pi.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext, NOOP_TELEMETRY_CONTEXT } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const cases = [];
for (const scope of ["memory", "noop", "late"]) for (const phase of ["ok", "error", "panic", "nil_panic", "nested_ok", "nested_error", "nested_panic", "snapshot"]) for (const fail of [false, true]) {
  const recorder = new InMemoryTelemetryContext();
  const context = scope === "noop" ? NOOP_TELEMETRY_CONTEXT : recorder;
  let captured: any, reads = 0, panicked = false, callbackError = false;
  const snapshots: any[] = [];
  const failure = new Error("callback failed");
  const status = {
    get status() {
      reads++;
      if (phase.startsWith("nested_")) captured.setStatus({status: "error", error: {name: "Nested", message: "inner"}});
      if (phase === "snapshot") snapshots.push(recorder.getSpans());
      if (phase === "nil_panic") throw null;
      if (phase === "panic" || phase === "nested_panic") throw new Error("read failed");
      return phase === "error" || phase === "nested_error" ? "error" : "ok";
    },
    error: {name: "Read", message: "outer"},
  };
  try {
    await context.startSpan({name: "read"}, span => {
      captured = span;
      if (scope !== "late") span.setStatus(status as any);
      if (fail) throw failure;
    });
  } catch (error) {
    callbackError = error === failure;
    panicked = !callbackError;
  }
  if (scope === "late") {
    try { captured.setStatus(status); } catch { panicked = true; }
  }
  cases.push({input: {scope, phase, fail}, expected: {reads, panicked, callbackError, snapshots, spans: recorder.getSpans()}});
}
const serialized = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-status-readers.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Status reader fixtures changed");
} else writeFileSync(destination, serialized);
console.log(`Verified ${cases.length} status reader cases against Pi ${manifest.commit}`);
