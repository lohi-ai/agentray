// Development oracle: parse external JSON, then exercise Pi's actual recorder.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const inputs = [
  '{}', 'null', '{"value":null}', '{"value":1e400}', '{"value":-1e400}',
  '{"value":-0}', '{"value":-1e-400}', '{"value":1e-400}',
  '{"value":9007199254740993}', '{"value":18446744073709551615}',
  '{"value":1.7976931348623157e308}', '{"value":1.7976931348623159e308}',
  '{"value":5e-324}', '{"value":2.4703282292062327e-324}',
  '{"value":2.4703282292062328e-324}',
  '{"value":[1e400,-1e400,-0,9007199254740993,null,true,"1e400"]}',
  '{"value":{"nested":[{"overflow":1e400,"zero":-0}],"finite":9007199254740993}}',
  '{"value":1e400,"value":42,"duplicate":{"n":-1e400,"n":-0}}',
  '{"__proto__":{"value":1e400},"constructor":-0,"toString":9007199254740993}',
  '{"value":{"__proto__":{"value":1e400},"constructor":-0},"0":1e400,"10":-0,"2":null}',
];
function numbers(value: any): Record<string,string> {
  const result: Record<string,string> = {};
  const visit = (v: any, path: string) => {
    if (typeof v === "number") {
      const bytes = new DataView(new ArrayBuffer(8)); bytes.setFloat64(0, v);
      result[path] = [...new Uint8Array(bytes.buffer)].map(b => b.toString(16).padStart(2,"0")).join("");
    } else if (v !== null && typeof v === "object") {
      for (const [key, child] of Object.entries(v)) visit(child, `${path}/${key}`);
    }
  };
  visit(value, ""); return result;
}
const spanNumbers = (spans: any[]) => numbers(spans.map(s => ({attributes: s.attributes, events: s.events.map((e: any) => e.attributes)})));
const copy = (value: any) => JSON.parse(JSON.stringify(value));
const cases = [];
for (const raw of inputs) for (const phase of ["start", "merge", "event"]) {
  const decoded = JSON.parse(raw);
  const recorder = new InMemoryTelemetryContext();
  let active: any;
  await recorder.startSpan({name: "decoded", attributes: phase === "start" ? decoded : {kept: true, value: 7}}, span => {
    if (phase === "merge") span.setAttributes(decoded);
    if (phase === "event") span.addEvent("decoded", decoded);
    const spans = recorder.getSpans();
    active = {spans: copy(spans), numbers: spanNumbers(spans)};
  });
  const spans = recorder.getSpans();
  cases.push({raw, phase, expected: {decodedNumbers: numbers(decoded), active, settled: {spans: copy(spans), numbers: spanNumbers(spans)}}});
}
const serialized = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-attribute-decode.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Attribute decode fixtures changed");
} else writeFileSync(destination, serialized);
console.log(`Verified ${cases.length} attribute decode cases against Pi ${manifest.commit}`);
