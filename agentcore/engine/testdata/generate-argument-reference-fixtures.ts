// Original-source oracle; production uses only the Go implementation.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop, runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const clone = (value: any) => JSON.parse(JSON.stringify(value));
const cases = [];
for (const mode of ["programmatic", "sequential", "parallel"]) {
  for (const shape of ["object", "array", "number"]) {
    const phases = shape === "number" ? ["before_replace", "execute_replace", "after_replace"] : ["before_replace", "before_null", "before_mutate", "before_mutate_replace", "before_replace_mutate", "execute_mutate", "execute_replace", "after_mutate", "after_replace"];
    if (shape === "object") phases.push("update_mutate", "late_before_mutate", "late_before_replace");
    for (const phase of phases) {
      const input = {mode, shape, phase};
      const snapshots: any[] = [];
      const args = shape === "object" ? {value: "original"} : shape === "array" ? ["original"] : 7;
      const replacement = () => shape === "object" ? {replacement: true} : shape === "array" ? ["replacement"] : 99;
      const mutate = (value: any) => { if (Array.isArray(value)) value.push("changed"); else value.changed = true; };
      let beforeContext: any, selected: any, beforeLocal: any, executed: any, executeLocal: any, afterLocal: any;
      const snapshotsOf = (stage: string, value: any) => snapshots.push({stage, args: clone(value)});
      const hooks = {
        beforeToolCall: (value: any) => {
          beforeContext = value;
          selected = value.args;
          snapshotsOf("before", value.args);
          if (phase === "before_mutate" || phase === "before_mutate_replace") mutate(value.args);
          if (["before_replace", "before_mutate_replace", "before_replace_mutate"].includes(phase)) value.args = replacement();
          if (phase === "before_null") value.args = null;
          if (phase === "before_replace_mutate") mutate(value.args);
          beforeLocal = value.args;
        },
        afterToolCall: (value: any) => {
          snapshotsOf("after", value.args);
          if (phase === "after_mutate") mutate(value.args);
          if (phase === "after_replace") value.args = replacement();
          afterLocal = value.args;
        },
      };
      const update = () => {
        if (phase === "update_mutate" || phase === "late_before_mutate") mutate(beforeContext.args);
        if (phase === "late_before_replace") beforeContext.args = replacement();
      };
      const tool: any = {name: "echo", label: "echo", description: "echo", parameters: {type: shape}, execute: async (_id: string, value: any, _signal: any, onUpdate: any) => {
        executed = value;
        snapshotsOf("execute", value);
        if (phase === "execute_mutate") mutate(value);
        if (phase === "execute_replace") value = replacement();
        executeLocal = value;
        onUpdate({content: [], details: {}});
        snapshotsOf("execute_after_update", value);
        return {content: [], details: {}, terminate: true};
      }};
      const call = {type: "toolCall", name: "echo", id: "first", arguments: args};
      const assistant: any = {role: "assistant", content: [call], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "toolUse", timestamp: 1};
      if (mode === "programmatic") {
        await runToolCall(call as any, {tools: [tool], assistantMessage: assistant, context: {messages: [], tools: [tool]}, ...hooks, onUpdate: update});
      } else {
        const stream = () => {const s = new AssistantMessageEventStream(); s.push({type: "done", reason: "toolUse", message: assistant}); return s;};
        await runAgentLoop([], {messages: [], tools: [tool]}, {model: {id: "test", api: "test", provider: "test"} as any, convertToLlm: (m: any) => m, toolExecution: mode as any, ...hooks, finishTurn: () => ({action: "end"})}, (event: any) => {if (event.type === "tool_execution_update") update();}, undefined, stream as any);
      }
      cases.push({input, expected: {snapshots, selected, beforeLocal, executed, executeLocal, afterLocal, lateBefore: beforeContext.args, raw: call.arguments, same: selected === executed}});
    }
  }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-argument-references.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi argument reference fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} argument reference cases against Pi ${manifest.commit}`);
