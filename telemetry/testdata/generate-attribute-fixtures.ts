// Development oracle only; Go tests load the checked-in JSON directly.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";

execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const destination = new URL("pi-attributes.json", import.meta.url);
type Value = { kind: string; value?: string; values?: Value[] };
const number = (value: string, kind = "float64"): Value => ({ kind, value });
const inputs: { name: string; key?: string; start: Value; update?: Value }[] = [
  { name: "byte array", start: { kind: "uint8[]", values: [number("0"), number("1"), number("255")] } },
  { name: "nil byte slice", start: { kind: "uint8[]" } },
  { name: "mixed Go numeric kinds", start: { kind: "any[]", values: [number("1", "int"), number("2.5"), number("3", "uint64")] } },
  { name: "mixed numeric widths", start: { kind: "any[]", values: [number("-128", "int8"), number("32767", "int16"), number("65535", "uint16"), number("0.5", "float32")] } },
  { name: "empty number array", start: { kind: "float64[]", values: [] } },
  { name: "not a number", start: number("NaN") },
  { name: "positive infinity", start: number("Infinity") },
  { name: "negative infinity", start: number("-Infinity") },
  { name: "negative zero", start: number("-0") },
  { name: "nonfinite numeric array", start: { kind: "float64[]", values: [number("NaN"), number("Infinity"), number("-Infinity"), number("-0"), number("1.25")] } },
  { name: "nonfinite merge", start: number("1"), update: number("NaN") },
  { name: "finite merge replaces nonfinite", start: number("Infinity"), update: number("2.5") },
  { name: "omitted merge retains nonfinite", start: number("NaN"), update: { kind: "omit" } },
  { name: "max signed integer", start: number("9223372036854775807", "int64") },
  { name: "min signed integer", start: number("-9223372036854775808", "int64") },
  { name: "max unsigned integer", start: number("18446744073709551615", "uint64") },
  { name: "integer beyond exact binary64", start: number("9007199254740993", "int64") },
  { name: "max finite number", start: number("1.7976931348623157e308") },
  { name: "min positive number", start: number("5e-324") },
  { name: "string array", start: { kind: "any[]", values: [{ kind: "string", value: "xin chào" }, { kind: "string", value: "42" }] } },
  { name: "boolean array", start: { kind: "any[]", values: [{ kind: "bool", value: "true" }, { kind: "bool", value: "false" }] } },
  { name: "prototype setter scalar", key: "__proto__", start: { kind: "string", value: "ignored" }, update: number("3") },
  { name: "prototype setter array", key: "__proto__", start: { kind: "float64[]", values: [number("1"), number("2")] }, update: { kind: "any[]", values: [{ kind: "string", value: "changed" }] } },
  { name: "constructor is an ordinary attribute", key: "constructor", start: number("1"), update: number("2") },
  { name: "toString is an ordinary attribute", key: "toString", start: { kind: "string", value: "kept" } },
  { name: "prototype is an ordinary attribute", key: "prototype", start: { kind: "bool", value: "false" } },
];
function value(spec: Value): any {
  if (spec.kind.endsWith("[]")) return (spec.values ?? []).map(value);
  if (spec.kind === "omit") return undefined;
  if (spec.kind === "string") return spec.value;
  if (spec.kind === "bool") return spec.value === "true";
  return Number(spec.value);
}
const cases = [];
for (const input of inputs) {
  const recorder = new InMemoryTelemetryContext();
  const key = input.key ?? "value";
  let active: unknown;
  await recorder.startSpan({ name: input.name, attributes: { [key]: value(input.start) } }, span => {
    span.addEvent("copy", { [key]: value(input.start) });
    if (input.update) span.setAttributes({ [key]: value(input.update) });
    active = recorder.getSpans();
  });
  cases.push({ ...input, expected: { active, settled: recorder.getSpans() } });
}
const output = JSON.stringify({ upstreamCommit: manifest.commit, cases }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi attribute fixtures differ; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} attribute cases against Pi ${manifest.commit}`);
