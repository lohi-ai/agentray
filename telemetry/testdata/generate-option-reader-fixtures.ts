// Development-only reference oracle; production telemetry is implemented in Go.
import {execFileSync} from "node:child_process";
import {readFileSync, writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/memory.ts";
import {NOOP_TELEMETRY_CONTEXT} from "../../third_party/pi/upstream/packages/telemetry/src/noop.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], {stdio: "inherit"});
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const cases = [];
for (const scope of ["root", "child", "noop", "late"]) {
  for (const phase of ["plain", "name_panic", "name_nil_panic", "attributes_panic", "value_panic", "attributes_change_name", "reader_child", "reader_child_panic"]) {
    for (const fail of [false, true]) {
      const input = {scope, phase, fail};
      const recorder = new InMemoryTelemetryContext();
      const reads: string[] = [], snapshots: any[] = [];
      const token = {value: "callback result"}, failure = new Error("callback failed");
      let parent: any = scope === "noop" ? NOOP_TELEMETRY_CONTEXT : recorder;
      let calls = 0, children = 0, valueSame = false, errorSame = false, unexpected = false, name = "read";
      const options = {
        get name() {
          reads.push("name");
          snapshots.push(recorder.getSpans());
          if (phase === "name_nil_panic") throw null;
          if (phase === "name_panic") throw new Error("name failed");
          return name;
        },
        get attributes(): any {
          reads.push("attributes");
          if (phase.startsWith("reader_child")) void parent.startSpan({name: "reader-child"}, () => {});
          if (phase === "attributes_change_name") name = "changed";
          if (phase === "attributes_panic" || phase === "reader_child_panic") throw new Error("attributes failed");
          return {get value() {
            reads.push("value");
            if (phase === "value_panic") throw new Error("value failed");
            return "read";
          }};
        },
      };
      const run = async () => {
        try {
          const value = await parent.startSpan(options, async (span: any) => {
            calls++;
            span.addEvent("callback", {value: true});
            await span.startSpan({get name() {reads.push("child-name"); return "callback-child";}}, () => {children++;});
            if (fail) throw failure;
            return token;
          });
          valueSame = value === token;
        } catch (error) {errorSame = error === failure; unexpected = !errorSame;}
      };
      if (scope === "child") await recorder.startSpan({name: "parent"}, async span => {parent = span; await run();});
      else if (scope === "late") {await recorder.startSpan({name: "parent"}, span => {parent = span;}); await run();}
      else await run();
      await recorder.startSpan({name: "probe"}, () => {});
      cases.push({input, expected: {reads, snapshots, calls, children, valueSame, errorSame, unexpected, spans: recorder.getSpans()}});
    }
  }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-option-readers.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi option reader fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} option reader cases against Pi ${manifest.commit}`);
