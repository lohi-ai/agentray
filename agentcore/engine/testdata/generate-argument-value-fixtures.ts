// Original-source JSON.parse/validation oracle, including in-memory number bits.
import {readFileSync, writeFileSync} from "node:fs";
import {root, manifest} from "./oracle.ts";
const {runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const raws = [
 "0", "-0", "-1e-9999", "1e400", "-1e400", "5e-324", "9007199254740993",
 '"\\ud800"', '"\\udc00"', '"\\ud800x"', '"\\ud83d\\ude80"', '"\\ufffd"',
 '{"\\ud800":"\\udc00","\\ud801":"\\ud800","value":-0}',
 '[1e400,-1e-9999,{"value":"\\ud800"}]',
 '"-0"', '"1e400"', 'null',
];
const schemas = [
 {},
 {type: "number"},
 {type: "integer"},
 {type: "string"},
 {type: "string", minLength: 2},
 {type: "number", minimum: 0, maximum: 10},
 {anyOf: [{type: "number"}, {type: "string"}]},
 {anyOf: [{type: "object", properties: {value: {type: "string"}}, required: ["value"]}, {type: "array", items: {type: "string"}}]},
 {type: "string", maxLength: 1},
 {type: "string", pattern: "^.$"},
 {type: "string", pattern: "^\\ud800$"},
 {type: "string", pattern: "^\\p{Cs}$"},
 {type: "string", pattern: "^\ud800$"},
 {type: "object", properties: {"\ud800": {type: "string", minLength: 2}, "\ud801": {type: "string", minLength: 2}}},
 {type: "object", required: ["\ud800", "\ud801"]},
 {type: "object", propertyNames: {pattern: "^[a-z]+$"}},
 {type: "object", patternProperties: {"^\\p{Cs}$": {type: "string", minLength: 2}}},
 {minimum: 0, maximum: 10, exclusiveMinimum: 0, exclusiveMaximum: 10, multipleOf: 3},
 {const: 1},
 {enum: [1, null, "\ud800"]},
 {type: ["number", "null"]},
 {type: "array", uniqueItems: true, contains: {type: "number"}},
 {dependencies: {"\ud800": ["\ud802", "\ud803"]}},
 {dependentRequired: {"\ud800": ["\ud802", "\ud803"]}},
 {type: "string", pattern: "^\\\ud800$"},
 {type: "string", pattern: "^[\ud800-\udfff]$"},
];
function inspect(value: any) {
 const numbers: any[] = [];
 const visit = (v: any, path: (string|number)[]) => {
  if (typeof v === "number") {
   const bits = new DataView(new ArrayBuffer(8));
   bits.setFloat64(0, v);
   numbers.push([JSON.stringify(path), bits.getBigUint64(0).toString(16).padStart(16, "0")]);
  } else if (Array.isArray(v)) v.forEach((c, i) => visit(c, [...path, i]));
  else if (v !== null && typeof v === "object") for (const k of Object.keys(v)) visit(v[k], [...path, k]);
 };
 visit(value, []);
 return {wire: JSON.stringify(value), numbers};
}
const cases = [];
for (const [r, raw] of raws.entries()) for (const [s, schema] of schemas.entries()) for (const wrapped of [false, true]) {
 const input = {name: `${r}/${s}/${wrapped}`, raw: wrapped ? `{"value":${raw}}` : raw, schema: wrapped ? {type: "object", properties: {value: schema}, required: ["value"]} : schema};
 let before: any = null, executed: any = null;
 const tool: any = {name: "echo", label: "echo", description: "echo", parameters: input.schema, execute: async (_id: string, args: any) => {executed = inspect(args); return {content: [{type: "text", text: "done"}], details: {}};}};
 const call: any = {type: "toolCall", id: "call", name: "echo", arguments: JSON.parse(input.raw)};
 const result = await runToolCall(call, {tools: [tool], assistantMessage: {}, context: {messages: [], tools: [tool]}, beforeToolCall: (c: any) => {before = inspect(c.args);}});
 cases.push({input, expected: {before, executed, isError: result.isError, text: JSON.stringify(result.result.content[0].text), wireText: JSON.stringify(JSON.parse(JSON.stringify(result.result)).content[0].text), source: JSON.stringify(call.arguments)}});
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-argument-values.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Argument value fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} argument value cases against Pi ${manifest.commit}`);
