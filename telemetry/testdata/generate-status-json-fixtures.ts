// Development oracle only; the Go runtime uses no TypeScript implementation.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const inputs: [string, any, string?][] = [
  ["null", null], ["false", false], ["true", true], ["zero", 0], ["number", 3],
  ["empty string", ""], ["string", "ok"], ["array", []], ["nonempty array", ["ok"]], ["empty object", {}],
  ["null status", {status: null}], ["boolean status", {status: true}], ["number status", {status: 3}],
  ["array status", {status: ["ok"]}], ["object status", {status: {value: "ok"}}],
  ["empty error object", {status: "error", error: {}}],
  ["name only", {status: "error", error: {name: "Named"}}],
  ["message only", {status: "error", error: {message: "message"}}],
  ["null error fields", {status: "error", error: {name: null, message: null}}],
  ["empty error fields", {status: "error", error: {name: "", message: ""}}],
  ["mixed error fields", {status: "error", error: {name: 42, message: false}}],
  ["structured error fields", {status: "error", error: {name: [1, null], message: {nested: "value"}, ignored: "drop"}}],
  ["false error", {status: "error", error: false}], ["zero error", {status: "error", error: 0}],
  ["empty string error", {status: "error", error: ""}], ["true error", {status: "error", error: true}],
  ["number error", {status: "error", error: 3}], ["string error", {status: "error", error: "message"}],
  ["array error", {status: "error", error: []}], ["nonempty array error", {status: "error", error: ["name", "message"]}],
  ["ok ignores malformed error", {status: "ok", error: {name: null, message: {value: "ignored"}}}],
  ["missing status with error", {error: {name: "Named", message: "message"}}],
];
inputs.push(
  ["rounded error number", null, '{"status":"error","error":{"name":9007199254740993,"message":-0}}'],
  ["overflow error fields", null, '{"status":"error","error":{"name":1e999,"message":-1e999}}'],
  ["surrogate error fields", null, String.raw`{"status":"error","error":{"name":"\ud800","message":"\ud83d\ude00"}}`],
  ["ordered nested error fields", null, '{"status":"error","error":{"name":{"10":1,"2":2,"b":9007199254740993,"a":0,"b":1e999},"message":[1e999,-0,1.00]}}'],
  ["overflow error is truthy", null, '{"status":"error","error":1e999}'],
  ["underflow error is falsy", null, '{"status":"error","error":1e-999}'],
);
const cases = [];
for (const [name, value, statusJSON] of inputs) for (const prior of ["none", "ok", "error"]) for (const fail of [false, true]) {
  const status = statusJSON ? JSON.parse(statusJSON) : value;
  const recorder = new InMemoryTelemetryContext();
  let active: any, failure: any;
  try {
    await recorder.startSpan({name: "status"}, span => {
      if (prior !== "none") span.setStatus(prior === "ok" ? {status: "ok"} : {status: "error", error: {name: "Prior", message: "retained"}});
      span.setStatus(status);
      active = recorder.getSpans();
      if (fail) throw new Error("callback failed");
    });
  } catch (error) { failure = (error as Error).message; }
  const settled = recorder.getSpans();
  const fields: Record<string, string> = {};
  const error = (settled[0].status as any).error;
  for (const name of ["name", "message"]) if (error && error[name] !== undefined) fields[name] = JSON.stringify(error[name]);
  cases.push({input: {name, status, ...(statusJSON ? {statusJSON} : {}), prior, fail}, expected: {active, settled, fields, statusJSON: JSON.stringify(settled[0].status), ...(failure ? {failure} : {})}});
}
const serialized = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-status-json.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Status JSON fixtures changed");
} else writeFileSync(destination, serialized);
console.log(`Verified ${cases.length} status JSON cases against Pi ${manifest.commit}`);
