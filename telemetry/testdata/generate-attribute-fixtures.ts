// Development oracle only; Go tests load the checked-in JSON directly.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";

execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const destination = new URL("pi-attributes.json", import.meta.url);
type Value = { kind: string; value?: string; values?: Value[]; entries?: Record<string, Value> };
const number = (value: string, kind = "float64"): Value => ({ kind, value });
const inputs: { name: string; key?: string; inspectFunctions?: boolean; start: Value; update?: Value }[] = [
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
for (const [name, data] of [
  ["mixed strings and numbers", [1, "text"]],
  ["mixed booleans and numbers", [1, true]],
  ["null array element", [1, null]],
  ["nested numeric arrays", [[1], [2, 3]]],
  ["object attribute", {nested: 1}],
  ["object array", [{nested: 1}, {nested: 2}]],
  ["empty object array", []],
  ["nested null field", {nested: null}],
  ["nested mixed values", [{nested: [1, true, "x", null]}]],
  ["nested special keys", JSON.parse('{"__proto__":{"value":1},"constructor":2}')],
] as [string, any][]) {
  const spec = {kind: "json", value: JSON.stringify(data)};
  inputs.push({name, start: spec});
  inputs.push({name: `merge ${name}`, start: number("1"), update: spec});
}
for (const [name, primitive] of [["nonfinite", number("Infinity")], ["integer rounding", number("9223372036854775807", "int64")]] as [string, Value][]) {
  inputs.push({name: `nested numeric array/${name}`, start: {kind: "any[]", values: [primitive, {kind: "any[]", values: [primitive]}]}});
  inputs.push({name: `nested numeric object/${name}`, start: {kind: "object", entries: {nested: primitive}}});
}
const nullValue: Value = {kind: "json", value: "null"};
const omittedValue: Value = {kind: "omit"};
const nestedOmission: Value = {kind: "object", entries: {kept: nullValue, omitted: omittedValue}};
inputs.push(
  {name: "top-level null", start: nullValue},
  {name: "null merge replaces value", start: number("1"), update: nullValue},
  {name: "value merge replaces null", start: nullValue, update: number("2")},
  {name: "omitted merge retains null", start: nullValue, update: omittedValue},
  {name: "omitted attribute", start: omittedValue},
  {name: "nested omitted field", start: nestedOmission},
  {name: "omitted array element", start: {kind: "any[]", values: [nullValue, omittedValue, number("1")]}},
  {name: "nested omitted array/object", start: {kind: "any[]", values: [nestedOmission, {kind: "any[]", values: [omittedValue]}]}},
  {name: "merge nested omitted field", start: number("1"), update: nestedOmission},
  {name: "prototype setter null", key: "__proto__", start: nullValue},
  {name: "nested prototype null", start: {kind: "object", entries: JSON.parse('{"__proto__":{"kind":"json","value":"null"},"omitted":{"kind":"omit"}}')}},
);
const callable: Value = {kind: "function"};
for (const input of [
  {name: "function attribute", start: callable},
  {name: "function merge replaces number", start: number("1"), update: callable},
  {name: "number merge replaces function", start: callable, update: number("2")},
  {name: "undefined merge retains function", start: callable, update: omittedValue},
  {name: "nested function object", start: {kind: "object", entries: {callback: callable, kept: number("1")}}},
  {name: "function array element", start: {kind: "any[]", values: [callable, number("1")]}},
  {name: "nested function arrays and objects", start: {kind: "any[]", values: [{kind: "object", entries: {callback: callable}}, {kind: "any[]", values: [callable]}]}},
  {name: "prototype setter function", key: "__proto__", start: callable},
] as typeof inputs) inputs.push({...input, inspectFunctions: true});

function functionPaths(spans: any[]): string[] {
  const paths: string[] = [];
  function visit(value: any, path: string) {
    if (typeof value === "function") paths.push(path);
    else if (value !== null && typeof value === "object") {
      for (const [key, child] of Object.entries(value)) visit(child, `${path}.${key}`);
    }
  }
  spans.forEach((span, i) => {
    visit(span.attributes, `${i}.attributes`);
    span.events.forEach((event: any, j: number) => visit(event.attributes, `${i}.events.${j}.attributes`));
  });
  return paths.sort();
}
function value(spec: Value): any {
  if (spec.kind === "object") return Object.fromEntries(Object.entries(spec.entries ?? {}).map(([key, child]) => [key, value(child)]));
  if (spec.kind === "json") return JSON.parse(spec.value!);
  if (spec.kind.endsWith("[]")) return (spec.values ?? []).map(value);
  if (spec.kind === "function") return () => { throw new Error("attribute function invoked"); };
  if (spec.kind === "omit") return undefined;
  if (spec.kind === "string") return spec.value;
  if (spec.kind === "bool") return spec.value === "true";
  return Number(spec.value);
}
const cases = [];
for (const input of inputs) {
  const recorder = new InMemoryTelemetryContext();
  const key = input.key ?? "value";
  let active: any[] = [];
  await recorder.startSpan({ name: input.name, attributes: { [key]: value(input.start) } }, span => {
    span.addEvent("copy", { [key]: value(input.start) });
    if (input.update) span.setAttributes({ [key]: value(input.update) });
    active = recorder.getSpans();
  });
  const settled = recorder.getSpans();
  const functions = input.inspectFunctions ? {active: functionPaths(active), settled: functionPaths(settled)} : undefined;
  cases.push({ ...input, expected: { active, settled, functions } });
}
const graphs = [];
for (const kind of ["object", "array", "cyclic-object", "cyclic-array"]) {
  const recorder = new InMemoryTelemetryContext();
  let original: any;
  if (kind === "object") original = {value: 1, nested: {value: 2}};
  if (kind === "array") original = [1, {value: 2}, [3]];
  if (kind === "cyclic-object") { original = {}; original.self = original; }
  if (kind === "cyclic-array") { original = ["original"]; original.push(original); }
  let exportFailed = false;
  await recorder.startSpan({name: kind, attributes: {value: original}}, span => {
    span.addEvent("copy", {value: original});
    const snapshot: any = recorder.getSpans()[0].attributes.value;
    if (kind.startsWith("cyclic")) {
      try { JSON.stringify(recorder.getSpans()); } catch { exportFailed = true; }
      if (kind === "cyclic-object") original.self = "cleared";
      else original[1] = "cleared";
    } else if (kind === "object") {
      original.value = 3;
      snapshot.nested.value = 4;
    } else {
      original[0] = 9;
      snapshot[0] = 8;
      snapshot[1].value = 4;
      snapshot[2][0] = 5;
    }
  });
  graphs.push({kind, expected: {exportFailed, spans: recorder.getSpans()}});
}
const output = JSON.stringify({ upstreamCommit: manifest.commit, cases, graphs }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi attribute fixtures differ; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} attribute cases and ${graphs.length} graph cases against Pi ${manifest.commit}`);
