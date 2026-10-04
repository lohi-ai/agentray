// Original Pi execution and reference-identity oracle; development only.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop, runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const clone = (value: any) => JSON.parse(JSON.stringify(value));
const cases = [];
for (const mode of ["programmatic", "sequential", "parallel"]) for (const shape of ["object", "array"]) for (const decoded of [false, true]) {
 for (const phase of ["plain", "update_edit", "update_replace", "after_edit", "after_empty_override", "after_content_override", "after_details_override", "after_structured_override", "after_null_override", "after_null_content_override", "after_result_clear", "end_edit", "message_edit", "settled_edit"]) {
  if (mode === "programmatic" && ["end_edit", "message_edit"].includes(phase)) continue;
  const input = {mode, shape, phase, decoded};
  const leaf = (text: string) => shape === "object" ? {child: {text}} : [{text}];
  let shared = leaf("original");
  const replacement = () => ({branch: leaf("replacement")});
  const edit = (value: any) => {
   (shape === "array" ? value[0] : value.child).text = "changed";
   if (shape === "array") value.push({text: "appended"});
   else value.added = true;
  };
  let original: any = {content: [{type: "text", text: "done"}], details: {branch: shared}, structuredContent: {branch: shared}, terminate: true};
  if (decoded) {original = clone(original); shared = original.details.branch;}
  const partial: any = {content: [], details: original.details, structuredContent: original.structuredContent};
  const snapshots: any[] = [];
  const capture = (stage: string, result: any) => snapshots.push({stage, result: clone(result)});
  let end: any, afterResult: any, messages: any[] = [];
  const update = (value: any) => {
   capture("update", value);
   if (phase === "update_edit") edit(value.details.branch);
   if (phase === "update_replace") value.details = replacement();
  };
  const afterToolCall = (value: any) => {
   afterResult = value.result;
   capture("after", value.result);
   if (phase === "after_edit") edit(value.result.details.branch);
   if (phase === "after_result_clear") {value.result.details = undefined; value.result.structuredContent = null;}
   if (phase === "after_empty_override") return {};
   if (phase === "after_content_override") return {content: []};
   if (phase === "after_details_override") return {details: replacement()};
   if (phase === "after_structured_override") return {structuredContent: replacement()};
   if (phase === "after_null_override") return {details: null, structuredContent: null};
   if (phase === "after_null_content_override") return {details: null, structuredContent: null, content: []};
  };
  const tool: any = {name: "echo", label: "echo", description: "echo", parameters: {type: "object"}, execute: async (_id: string, _args: any, _signal: any, onUpdate: any) => {onUpdate(partial); return original;}};
  const call: any = {type: "toolCall", id: "call", name: "echo", arguments: {}};
  const assistant: any = {role: "assistant", content: [call], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "toolUse", timestamp: 1};
  if (mode === "programmatic") {
   const outcome = await runToolCall(call, {tools: [tool], assistantMessage: assistant, context: {messages: [], tools: [tool]}, onUpdate: update, afterToolCall});
   end = outcome.result;
  } else {
   const stream = () => {const s = new AssistantMessageEventStream(); s.push({type: "done", reason: "toolUse", message: assistant}); return s;};
   const result = await runAgentLoop([], {messages: [], tools: [tool]}, {model: {id: "test", api: "test", provider: "test"} as any, convertToLlm: (m: any) => m, toolExecution: mode as any, afterToolCall, finishTurn: () => ({action: "end"})}, (event: any) => {
    if (event.type === "tool_execution_update") update(event.partialResult);
    if (event.type === "tool_execution_end") {end = event.result; capture("end", end); if (phase === "end_edit") edit(end.details.branch);}
    if (event.type === "message_start" && event.message.role === "toolResult") {capture("message", event.message); if (phase === "message_edit") edit(event.message.details.branch);}
   }, undefined, stream as any);
   messages = result.filter((m: any) => m.role === "toolResult");
  }
  if (phase === "settled_edit") edit(shared);
  cases.push({input, expected: {snapshots, original, partial, end, messages, shared, identity: {
   after: afterResult === original, end: end === original,
   details: end.details === original.details,
   structured: end.structuredContent === original.structuredContent,
   nested: end.details?.branch === shared,
   message: mode === "programmatic" ? null : messages[0].details === end.details
  }}});
 }
}
const values = [];
for (const [index, raw] of ["missing", "null", "false", "0", "-0", '""', '"\\ud800"', '{"z":1e400,"a":-0}', '[null,-0,"\\ud800"]', "1e400", "function"].entries()) {
 for (const phase of ["plain", "empty_override", "null_override", "content_override", "primitive_override", "clear"]) {
  const encoded = (raw === "missing" || raw === "function") ? '{"content":[]}' : `{"content":[],"details":${raw},"structuredContent":${raw}}`;
  const original = JSON.parse(encoded);
  if (raw === "function") {original.details = () => {}; original.structuredContent = () => {};}
  const tool: any = {name: "echo", label: "echo", description: "echo", parameters: {type: "object"}, execute: async () => original};
  const result = await runToolCall({type: "toolCall", name: "echo", id: "call", arguments: {}} as any, {tools: [tool], assistantMessage: {} as any, context: {messages: [], tools: [tool]}, afterToolCall: (c: any) => {
   if (phase === "empty_override") return {};
   if (phase === "null_override") return {details: null, structuredContent: null};
   if (phase === "content_override") return {content: []};
   if (phase === "primitive_override") return {details: false, structuredContent: 0};
   if (phase === "clear") {c.result.details = undefined; c.result.structuredContent = null;}
  }});
  const describe = (v: any) => {
   if (v === undefined) return {kind: "undefined"};
   if (v === null) return {kind: "null"};
   if (typeof v === "number") {const view = new DataView(new ArrayBuffer(8)); view.setFloat64(0, v); return {kind: "number", wire: JSON.stringify(v), bits: view.getBigUint64(0).toString(16).padStart(16, "0")};}
   return {kind: typeof v, wire: JSON.stringify(v)};
  };
  values.push({input: {name: `${index}/${phase}`, raw: encoded, phase, function: raw === "function"}, expected: {wire: JSON.stringify(result.result), details: describe(result.result.details), structured: describe(result.result.structuredContent)}});
 }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases, values}, null, 2) + "\n";
const destination = new URL("pi-result-values.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi result value fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} reference and ${values.length} JSON result value cases against Pi ${manifest.commit}`);
