// Development oracle: production tool selection/execution is native Go.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop, runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const cases = [];
for (const mode of ["programmatic", "sequential", "parallel", "agent_sequential", "agent_parallel", "agent_set_sequential", "agent_set_parallel"]) {
  for (const phase of ["plain", "before_mutate", "before_replace", "before_grow_mutate", "prepare_mutate", "prepare_replace", "prepare_grow_mutate", "next_before_mutate", "next_before_replace", "next_before_grow_mutate"]) {
    if (mode === "programmatic" && phase.startsWith("next_")) continue;
    const input = {mode, phase};
    const executed: any[] = [], results: any[] = [];
    const makeTool = (label: string): any => ({
      name: "echo", label, description: label, parameters: {type: "object"},
      execute: async (id: string) => {
        executed.push({id, executor: label});
        return {content: [{type: "text", text: label}], details: {}, terminate: true};
      },
    });
    const retained = makeTool("original");
    let activeTools = [retained];
    const mutate = (at: string) => {
      if (phase === at + "_replace") activeTools[0] = makeTool("replacement");
      if (phase === at + "_grow_mutate") {
        for (let i = 0; i < 8; i++) activeTools.push({...makeTool("padding"), name: `padding${i}`});
      }
      if (phase === at + "_mutate" || phase === at + "_grow_mutate") {
        Object.assign(retained, {label: "mutated", description: "mutated", execute: makeTool("mutated").execute});
      }
    };
    let prepared = false;
    retained.prepareArguments = (args: any) => { if (!prepared) { prepared = true; mutate("prepare"); } return args; };
    const assistant: any = {role: "assistant", content: [{type: "toolCall", id: "first", name: "echo", arguments: {}}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "toolUse", timestamp: 1};
    if (mode !== "programmatic") assistant.content.push({type: "toolCall", id: "second", name: "echo", arguments: {}});
    const model = {id: "test", api: "test", provider: "test"};
    const hooks = {
      beforeToolCall: (value: any) => { mutate(value.toolCall.id === "first" ? "before" : "next_before"); },
      prepareRequest: (request: any) => { activeTools = request.context.tools; },
      finishTurn: () => ({action: "end"}),
    };
    const streamFn = async () => { const s = new AssistantMessageEventStream(); s.push({type: "done", reason: "toolUse", message: assistant}); return s; };
    const emit = (event: any) => { if (event.type === "message_end" && event.message.role === "toolResult") results.push(event.message); };
    if (mode === "programmatic") {
      const outcome = await runToolCall(assistant.content[0], {tools: activeTools, assistantMessage: assistant, context: {messages: [assistant], tools: activeTools}, ...hooks});
      results.push({toolCallId: outcome.toolCall.id, content: outcome.result.content, isError: outcome.isError});
    } else if (mode.startsWith("agent_")) {
      const agent = new Agent({initialState: {tools: mode.includes("_set_") ? [] : activeTools, model}, streamFn, toolExecution: mode.split("_").at(-1), ...hooks});
      if (mode.includes("_set_")) agent.state.tools = activeTools;
      agent.subscribe(emit);
      await agent.prompt({role: "user", content: "go", timestamp: 0});
    } else {
      await runAgentLoop([{role: "user", content: "go", timestamp: 0}], {messages: [], tools: activeTools}, {model, convertToLlm: (m: any) => m, toolExecution: mode, ...hooks}, emit, undefined, streamFn);
    }
    executed.sort((a, b) => a.id.localeCompare(b.id));
    cases.push({input, expected: {executed, results, retained: retained.label, slot: activeTools[0].label, same: retained === activeTools[0]}});
  }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-tool-definitions.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi tool definition fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} tool definition cases against Pi ${manifest.commit}`);
