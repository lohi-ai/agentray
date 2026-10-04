// Observe the original Agent's ReadonlySet identity, ordering and settlement.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const cases = [];
for (const mode of ["sequential", "parallel"]) {
 for (const ids of [[], ["a"], ["a", "b"], ["a", "a"], ["b", "a", "b"], ["", "__proto__", "constructor"], ["10", "2", "1"]]) {
  for (const variant of ["success", "missing", "invalid", "blocked", "execute_error", "after_error", "truncated", "mutate_id", "abort_before", "listener_start_error", "listener_update_error", "listener_end_error", "listener_agent_end_error"]) {
   if (ids.length === 0 && variant !== "success" && variant !== "listener_agent_end_error") continue;
   // A failed parallel sibling need not settle before the Agent ends. Keep
   // those failure cases single-call; ordered multi-call runs use explicit gates.
   if (mode === "parallel" && ids.length > 1 && (variant.startsWith("listener_") || variant === "abort_before")) continue;
   const input = {mode, ids, variant};
   const observations: any[] = [], sets: Set<string>[] = [];
   let agent: any;
   const observe = (stage: string) => {
    const set = agent.state.pendingToolCalls;
    let ref = sets.indexOf(set);
    if (ref < 0) {ref = sets.length; sets.push(set);}
    observations.push({stage, ref, size: set.size, values: [...set], has: [...ids, "absent"].map(id => set.has(id)), wire: JSON.stringify(set), stateWire: JSON.stringify(JSON.parse(JSON.stringify(agent.state)).pendingToolCalls), repeatedGetter: set === agent.state.pendingToolCalls});
   };
   const gates = ids.map(() => {
    let resolve!: () => void;
    const promise = new Promise<void>(done => {resolve = done;});
    return {promise, resolve};
   });
   gates.at(-1)?.resolve();
   const result = () => ({content: [{type: "text", text: "ok"}], details: {}});
   const tools = ids.map((_id, index) => ({name: `tool${index}`, label: `tool${index}`, description: "", parameters: {type: "object"}, execute: async (_id: string, _args: any, _signal: any, update: any) => {
    if (mode === "parallel") await gates[index].promise;
    observe(`execute:${index}`);
    if (variant === "execute_error") throw new Error("execute failed");
    await update(result());
    return result();
   }}));
   const message = {role: "assistant", content: ids.map((id, index) => ({type: "toolCall", id, name: `tool${index}`, arguments: variant === "invalid" ? [] : {}})), ...model, model: model.id, usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: variant === "truncated" ? "length" : "stop", timestamp: 2};
   agent = new Agent({initialState: {model, tools: variant === "missing" ? [] : tools}, toolExecution: mode,
    streamFn: async () => {const s = new AssistantMessageEventStream(); s.push({type: "done", reason: message.stopReason, message}); return s;},
    beforeToolCall: (call: any) => {
     observe(`before:${call.toolCall.name}`);
     if (variant === "mutate_id") call.toolCall.id = `changed:${call.toolCall.name}`;
     if (variant === "blocked") return {block: true};
     if (variant === "abort_before") agent.abort();
    },
    afterToolCall: (call: any) => {observe(`after:${call.toolCall.name}`); if (variant === "after_error") throw new Error("after failed");},
    finishTurn: () => {observe("finish"); return {action: "end"};},
   } as any);
   observe("initial");
   let failed = false;
   const failureEvent = ({listener_start_error: "tool_execution_start", listener_update_error: "tool_execution_update", listener_end_error: "tool_execution_end", listener_agent_end_error: "agent_end"} as any)[variant];
   agent.subscribe((event: any) => {
    observe(`first:${event.type}:${event.toolName ?? event.message?.role ?? ""}`);
    if (!failed && event.type === failureEvent) {failed = true; throw new Error("listener failed");}
   });
   agent.subscribe((event: any) => {
    observe(`second:${event.type}:${event.toolName ?? event.message?.role ?? ""}`);
    if (event.type === "tool_execution_end") gates[Number(event.toolName.slice(4)) - 1]?.resolve();
   });
   await agent.prompt({role: "user", content: "go", timestamp: 1});
   observe("settled");
   agent.reset();
   observe("reset");
   cases.push({input, expected: {observations, retained: sets.map(set => [...set])}});
  }
 }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-pending-tools.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi pending-tool fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} pending-tool cases against Pi ${manifest.commit}`);
