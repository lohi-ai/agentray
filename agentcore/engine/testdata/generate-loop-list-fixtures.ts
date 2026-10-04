import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {runAgentLoop, runAgentLoopContinue, agentLoop, agentLoopContinue} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const user = (timestamp: number) => ({role: "user", content: "go", timestamp});
const summary = (list: any[]) => ({length: list.length, keys: Object.keys(list).map(Number), values: Array.from(list, m => m == null ? null : {role: m.role, timestamp: m.timestamp})});
const edit = (list: any[], action: string) => {
 if (action === "append") list.push(user(77));
 if (action === "replace") list[0] = user(77);
 if (action === "delete") delete list[0];
 if (action === "shrink") list.length = 0;
 if (action === "grow") list.length += 2;
 if (action === "nested" && list[0]) list[0].timestamp = 88;
};
const assistant = (number: number, stop: string) => ({role: "assistant", content: number === 1 && stop === "normal" ? [{type: "toolCall", id: "call", name: "echo", arguments: {}}] : [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: stop === "normal" ? "stop" : stop, timestamp: number + 1});
const response = (message: any) => {
 const s = new AssistantMessageEventStream();
 if (["error", "aborted"].includes(message.stopReason)) s.push({type: "error", reason: message.stopReason, error: message});
 else s.push({type: "done", reason: message.stopReason, message});
 return s;
};
const tool = () => ({name: "echo", label: "echo", description: "", parameters: {type: "object"}, execute: async () => ({content: [{type: "text", text: "ok"}], details: {}})});
const cases = [];
for (const mode of ["run", "continue", "agent"]) for (const stop of ["normal", "error", "aborted"]) for (const subject of ["newMessages", "toolResults"]) {
 for (const phase of stop === "normal" ? ["finish", "turn_end", "next", "request", "agent_end", "settled"] : ["finish", "turn_end", "agent_end", "settled"]) {
  for (const action of stop === "normal" ? ["append", "replace", "delete", "shrink", "grow", "nested"] : ["append", "replace", "shrink"]) {
   const input = {mode, stop, subject, phase, action};
   const observations: any[] = [], requests: any[] = [], lists: any[][] = [];
   let first: any, terminal: any[], result: any[], turns = 0, responseCount = 0, requestCount = 0, agent: any;
   const ref = (list: any[]) => {let index = lists.indexOf(list); if (index < 0) {index = lists.length; lists.push(list);} return index;};
   const describe = (list: any[]) => ({ref: ref(list), ...summary(list)});
   const observe = (stage: string, turn?: any) => observations.push({stage, retainedNew: describe(first.newMessages), retainedTools: describe(first.toolResults), ...(turn ? {currentNew: describe(turn.newMessages), currentTools: describe(turn.toolResults)} : {})});
   const mutate = (at: string) => {if (phase === at) edit(first[subject], action);};
   const config = {model, convertToLlm: (m: any) => m,
    finishTurn: (turn: any) => {
     turns++;
     if (!first) {first = turn; mutate("finish");}
     observe(`finish:${turns}`, turn);
     return {action: turns === 1 ? "continue" : "end"};
    },
    prepareNextTurn: (turn: any) => {mutate("next"); observe("next", turn); return {messages: [user(10)]};},
    prepareRequest: () => {if (requestCount++ > 0) {mutate("request"); observe("request");}},
   };
   const stream = async (_model: any, context: any) => {requests.push(summary(context.messages)); return response(assistant(++responseCount, stop));};
   const emit = (event: any) => {
    if (event.type === "turn_end") {
     if (turns === 1) mutate("turn_end");
     observe(`turn_end:${turns}`);
     observations.at(-1).eventTools = describe(event.toolResults);
    }
    if (event.type === "agent_end") {terminal = event.messages; mutate("agent_end"); observe("agent_end"); observations.at(-1).terminal = describe(terminal);}
   };
   const initial = {messages: mode === "continue" ? [user(1)] : [], tools: [tool()]};
   if (mode === "run") result = await runAgentLoop([user(1)], initial, config, emit, undefined, stream);
   if (mode === "continue") result = await runAgentLoopContinue(initial, config, emit, undefined, stream);
   if (mode === "agent") {
    agent = new Agent({initialState: {model, tools: initial.tools}, ...config, prepareNextTurn: undefined, prepareNextTurnWithContext: config.prepareNextTurn, streamFn: stream} as any);
    agent.subscribe(emit);
    await agent.prompt(user(1));
    result = terminal!;
   }
   mutate("settled");
   observe("settled");
   cases.push({input, expected: {requests, observations, result: describe(result!), terminal: describe(terminal!), retained: lists.map(summary), ...(agent ? {state: summary(agent.state.messages)} : {})}});
  }
 }
}
const streams = [];
for (const mode of ["run", "continue"]) for (const action of ["append", "replace", "delete", "shrink", "grow", "nested"]) {
 const config = {model, convertToLlm: (m: any) => m};
 const stream = mode === "run" ? agentLoop([user(1)], {messages: []}, config, undefined, async () => response(assistant(1, "stop"))) : agentLoopContinue({messages: [user(1)]}, config, undefined, async () => response(assistant(1, "stop")));
 const result = await stream.result();
 let terminal: any;
 for await (const event of stream) if (event.type === "agent_end") terminal = event.messages;
 edit(result, action);
 streams.push({input: {mode, action}, expected: {same: result === terminal, repeated: result === await stream.result(), result: summary(result), terminal: summary(terminal)}});
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases, streams}, null, 2) + "\n";
const destination = new URL("pi-loop-lists.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi loop-list fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} loop-list and ${streams.length} stream-result cases against Pi ${manifest.commit}`);
