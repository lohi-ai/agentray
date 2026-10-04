// Development-only oracle; execution under test is the unchanged pinned Pi source.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop, runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const clone = (v: any) => JSON.parse(JSON.stringify(v));
const cases = [];
for (const mode of ["programmatic", "sequential", "parallel"]) {
  for (const shape of ["object", "array"]) {
    for (const phase of ["before_edit", "before_replace", "execute_edit", "execute_replace", "update_edit", "after_edit", "after_replace", "settled_edit", "replace_then_update"]) {
      const input = {mode, shape, phase};
      const branch = () => shape === "object" ? {child: {value: "original"}} : [{value: "original"}];
      const replacement = () => shape === "object" ? {child: {value: "replacement"}} : [{value: "replacement"}];
      const edit = (v: any) => {
        (shape === "object" ? v.child : v[0]).value = "changed";
        if (shape === "array") v.push({value: "appended"});
        else v.added = true;
      };
      let selected: any, retained: any, executed: any, after: any;
      const snapshots: any[] = [];
      const capture = (stage: string, args: any) => snapshots.push({stage, args: clone(args), retained: clone(retained)});
      const hooks = {
        beforeToolCall: (v: any) => {
          selected = v.args;
          retained = v.args.branch;
          if (phase === "before_edit") edit(retained);
          if (phase === "before_replace" || phase === "replace_then_update") v.args.branch = replacement();
          capture("before", v.args);
        },
        afterToolCall: (v: any) => {
          after = v.args.branch;
          if (phase === "after_edit") edit(retained);
          if (phase === "after_replace") v.args.branch = replacement();
          capture("after", v.args);
        }
      };
      const update = () => {
        if (phase === "update_edit" || phase === "replace_then_update") edit(retained);
      };
      const tool: any = {name: "echo", label: "echo", description: "echo", parameters: {type: "object"}, execute: async (_id: string, args: any, _signal: any, onUpdate: any) => {
        executed = args.branch;
        if (phase === "execute_edit") edit(retained);
        if (phase === "execute_replace") args.branch = replacement();
        onUpdate({content: [], details: {}});
        capture("execute", args);
        return {content: [], details: {}, terminate: true};
      }};
      const call = {type: "toolCall", id: "first", name: "echo", arguments: {branch: branch()}};
      const assistant: any = {role: "assistant", content: [call], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "toolUse", timestamp: 1};
      if (mode === "programmatic") {
        await runToolCall(call as any, {tools: [tool], assistantMessage: assistant, context: {messages: [], tools: [tool]}, ...hooks, onUpdate: update});
      } else {
        const stream = () => {const s = new AssistantMessageEventStream(); s.push({type: "done", reason: "toolUse", message: assistant}); return s;};
        await runAgentLoop([], {messages: [], tools: [tool]}, {model: {id: "test", api: "test", provider: "test"} as any, convertToLlm: (m: any) => m, toolExecution: mode as any, ...hooks, finishTurn: () => ({action: "end"})}, (event: any) => {if (event.type === "tool_execution_update") update();}, undefined, stream as any);
      }
      if (phase === "settled_edit") edit(retained);
      cases.push({input, expected: {snapshots, selected, retained, executed, after, raw: call.arguments, sameExecute: retained === executed, sameAfter: retained === after, stillAttached: retained === selected.branch}});
    }
  }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-nested-arguments.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi nested argument fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} nested argument cases against Pi ${manifest.commit}`);
