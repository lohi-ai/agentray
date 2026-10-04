import {execFileSync} from "node:child_process";
import {readFileSync, writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], {stdio: "inherit"});
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const define = (array: any[], name: string, value: any) => Object.defineProperty(array, name, {value, enumerable: true, configurable: true, writable: true});
const describe = (array: any[], meta: any) => ({length: array.length, keys: Object.keys(array), wire: JSON.stringify(array), entries: Object.keys(array).map(name => {
 const value = (array as any)[name];
 return [name, value === undefined ? {kind: "undefined"} : value === null ? {kind: "null"} : typeof value === "function" ? {kind: "function"} : Array.isArray(value) ? {kind: "array", self: value === array} : typeof value === "object" ? {kind: "object", n: value.n, input: value === meta} : {kind: typeof value, value}];
})});
const cases = [];
for (const placement of ["outer", "object", "array", "shared", "status"]) for (const target of ["input", "snapshot"]) for (const phase of ["active", "settled"]) for (const operation of ["late", "overwrite", "reinsert", "shrink", "numeric", "nested", "cycle", "undefined", "function", "constructor", "proto", "max"]) {
 const input = {placement, target, phase, operation};
 const original: any[] = [1, 2], meta = {n: 1};
 for (const [name, value] of [["meta", meta], ["-1", 3], ["01", 4], ["constructor", "tag"], ["4294967295", 5], ["__proto__", meta]] as [string, any][]) define(original, name, value);
 const value = placement === "outer" ? original : placement === "array" ? [original] : placement === "shared" ? {left: original, right: original} : {inner: original};
 const select = (value: any): any[] => placement === "outer" ? value : placement === "array" ? value[0] : placement === "shared" ? value.left : value.inner;
 const arrays = (span: any): any[][] => placement === "status" ? [span.status.error.name, span.status.error.message] : [select(span.attributes.value), select(span.events[0].attributes.value)];
 const project = (spans: any) => ({wire: JSON.stringify(spans), arrays: spans.map((span: any) => {
  const [first, second] = arrays(span);
  return {first: describe(first, meta), second: describe(second, meta), same: first === second, firstInput: first === original, secondInput: second === original};
 })});
 const recorder = new InMemoryTelemetryContext();
 let before: any, after: any, retained: any, calls = 0, applied = true;
 const mutate = () => {
  const array: any = target === "input" ? original : arrays(retained[0])[0];
  if (operation === "late") define(array, "late", 7);
  if (operation === "overwrite") define(array, "meta", 7);
  if (operation === "reinsert") {delete array["01"]; define(array, "01", 8);}
  if (operation === "shrink") array.length = 0;
  if (operation === "numeric") define(array, "3", 4);
  if (operation === "nested") {if (array.meta) array.meta.n = 8; else applied = false;}
  if (operation === "cycle") define(array, "self", array);
  if (operation === "undefined") define(array, "note", undefined);
  if (operation === "function") define(array, "fn", () => {calls++;});
  if (operation === "constructor") define(array, "constructor", null);
  if (operation === "proto") define(array, "__proto__", {n: 9});
  if (operation === "max") define(array, "4294967295", 9);
 };
 await recorder.startSpan({name: "properties", ...(placement === "status" ? {} : {attributes: {value}})}, span => {
  if (placement === "status") span.setStatus({status: "error", error: {name: original, message: original}} as any);
  else span.addEvent("copy", {value});
  retained = recorder.getSpans(); before = project(retained);
  if (phase === "active") {mutate(); after = project(recorder.getSpans());}
 });
 if (phase === "settled") {mutate(); after = project(recorder.getSpans());}
 cases.push({input, expected: {before, after, retained: project(retained), settled: project(recorder.getSpans()), original: describe(original, meta), calls, applied}});
}
const clones = [];
for (const operation of ["plain", "clone_meta", "input_meta", "replace_meta", "delete_meta", "truncate", "sparse"]) {
 const meta = {n: 1}, array: any[] = [meta];
 define(array, "meta", meta); define(array, "self", array); define(array, "__proto__", meta); define(array, "constructor", null);
 if (operation === "sparse") {array.length = 3; delete array[0];}
 const root = {array, meta}, clone = structuredClone(root);
 if (operation === "clone_meta") (clone.array as any).meta.n = 2;
 if (operation === "input_meta") meta.n = 2;
 if (operation === "replace_meta") define(clone.array, "meta", {n: 3});
 if (operation === "delete_meta") delete (clone.array as any).meta;
 if (operation === "truncate") clone.array.length = 0;
 const inspect = (root: any) => ({array: describe(root.array, meta), meta: root.meta.n, same: root.array.meta === root.meta, proto: root.array.__proto__ === root.meta, self: root.array.self === root.array});
 clones.push({operation, expected: {original: inspect(root), clone: inspect(clone), detached: clone.array !== array && clone.meta !== meta}});
}
const output = `{"upstreamCommit":${JSON.stringify(manifest.commit)},"cases":[\n${cases.map(value => JSON.stringify(value)).join(",\n")}\n],"clones":${JSON.stringify(clones)}}\n`;
const destination = new URL("pi-array-properties.json", import.meta.url);
if (process.argv.includes("--check")) {if (readFileSync(destination, "utf8") !== output) throw new Error("Telemetry array-property fixtures differ");}
else writeFileSync(destination, output);
console.log(`Verified ${cases.length} telemetry array-property and ${clones.length} graph-clone cases at Pi ${manifest.commit}`);
