// Development-only oracle using the unchanged Pi recorder.
import {execFileSync} from "node:child_process";
import {readFileSync, writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/memory.ts";
import {NOOP_TELEMETRY_CONTEXT} from "../../third_party/pi/upstream/packages/telemetry/src/noop.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], {stdio: "inherit"});
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const cases = [];
for (const method of ["attributes", "event"]) for (const scope of ["memory", "noop", "late"]) {
  for (const phase of ["value", "panic", "nil_panic", "nested", "nested_panic", "snapshot", "nested_undefined", "nested_null"]) {
    for (const fail of scope === "memory" ? [false, true] : [false]) {
      const input = {method, scope, phase, fail};
      const recorder = new InMemoryTelemetryContext();
      const parent = scope === "noop" ? NOOP_TELEMETRY_CONTEXT : recorder;
      let captured: any, reads = 0, panicked = false, callbackError = false;
      const snapshots: any[] = [];
      const failure = new Error("callback failed");
      const attrs = {get outer() {
        reads++;
        if (phase.startsWith("nested")) {
          captured.setAttributes({inner: true, base: "changed"});
          captured.addEvent("inner", {value: 1});
          captured.setStatus({status: "error", error: {name: "Nested", message: "inner"}});
        }
        if (phase === "snapshot" || phase.startsWith("nested")) snapshots.push(recorder.getSpans());
        if (phase === "nil_panic") throw null;
        if (phase === "panic" || phase === "nested_panic") throw new Error("read failed");
        if (phase === "nested_undefined") return undefined;
        if (phase === "nested_null") return null;
        return "read";
      }};
      const apply = () => method === "attributes" ? captured.setAttributes(attrs) : captured.addEvent("outer", attrs);
      try {
        await parent.startSpan({name: "read", attributes: {base: "initial", kept: [1]}}, span => {
          captured = span;
          if (scope !== "late") apply();
          if (fail) throw failure;
        });
      } catch (error) { if (error === failure) callbackError = true; else panicked = true; }
      if (scope === "late") { try {apply();} catch {panicked = true;} }
      cases.push({input, expected: {reads, panicked, callbackError, snapshots, spans: recorder.getSpans()}});
    }
  }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-attribute-readers.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi attribute reader fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} attribute reader cases against Pi ${manifest.commit}`);
