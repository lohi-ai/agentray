import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {runAgentLoop, runAgentLoopContinue} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const user = (timestamp: number) => ({role: "user", content: "go", timestamp});
const message = (m: any) => m ? {role: m.role, timestamp: m.timestamp, tools: (m.toolsAdded ?? []).map((t: any) => t.name)} : null;
const values = (list: any[]) => Array.from(list, message);
const response = (number: number) => {
 const message = {role: "assistant", content: [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "stop", timestamp: number + 1};
 const stream = new AssistantMessageEventStream(); stream.push({type: "done", reason: "stop", message}); return stream;
};
const actions = ["append", "replace", "shrink", "truncate", "rebind", "nested"];
const cases = [];
for (const mode of ["run", "agent"]) for (const declaration of ["none", "insert", "rewrite"]) for (const phase of ["agent_start", "turn_start", "message_start", "message_end", "request", "settled"]) for (const action of actions) {
 const input = {kind: "prompt", mode, declaration, phase, action};
 let selected: any[] = [declaration === "rewrite" ? {role: "system", content: "", timestamp: 10} : user(10), user(11)];
 const retained = selected;
 let edited = false, agent: any, result: any[] = [];
 const mutate = (at: string) => {
  if (edited || phase !== at) return;
  edited = true;
  if (action === "append") retained.push(user(77));
  if (action === "replace") retained[1] = user(77);
  if (action === "shrink") retained.length = 0;
  if (action === "truncate") retained.length = 1;
  if (action === "rebind") selected = [user(77)];
  if (action === "nested") retained[1].timestamp = 88;
 };
 const events: any[] = [], requests: any[] = [];
 const tools = declaration === "none" ? [] : [{name: "echo", label: "echo", description: "", parameters: {type: "object"}, execute: async () => ({content: [], details: {}})}];
 const emit = (event: any) => {
  mutate(event.type);
  events.push({type: event.type, ...(event.message ? {message: message(event.message)} : {}), ...(event.messages ? {messages: values(event.messages)} : {})});
  if (event.type === "agent_end") result = event.messages;
 };
 const config = {model, convertToLlm: (m: any[]) => m, prepareRequest: () => {mutate("request");}, finishTurn: () => ({action: "end"})};
 const stream = async (_model: any, context: any) => {requests.push(values(context.messages)); return response(1);};
 if (mode === "run") result = await runAgentLoop(selected, {messages: [], tools}, config, emit, undefined, stream);
 else {
  agent = new Agent({initialState: {model}, ...config, streamFn: stream} as any);
  agent.state.tools = tools; agent.subscribe(emit); await agent.prompt(selected);
 }
 mutate("settled");
 cases.push({input, expected: {events, requests, result: values(result), selected: values(selected), retained: values(retained), same: selected === retained, edited, ...(agent ? {state: values(agent.state.messages)} : {})}});
}
for (const mode of ["run", "continue", "agent"]) for (const subject of ["initial", "steering", "followup", "prepared"]) {
 if (mode === "agent" && subject !== "prepared") continue;
 for (const phase of ["poll", "next", "turn_start", "message_start", "message_end", "request", "settled"]) for (const action of actions) {
  const input = {kind: "pending", mode, subject, phase, action};
  let selected: any[] = [user(10), user(11)];
  const retained = selected;
  let edited = false, agent: any, result: any[] = [], turns = 0, requested = 0, finished = 0, steering = 0, followup = 0;
  const targetTurn = subject === "initial" ? 1 : 2;
  const mutate = (at: string) => {
   if (edited || phase !== at) return;
   edited = true;
   if (action === "append") retained.push(user(77));
   if (action === "replace") retained[1] = user(77);
   if (action === "shrink") retained.length = 0;
   if (action === "truncate") retained.length = 1;
   if (action === "rebind") selected = [user(77)];
   if (action === "nested") retained[1].timestamp = 88;
  };
  const events: any[] = [], requests: any[] = [], hooks: any[] = [];
  const emit = (event: any) => {
   if (event.type === "turn_start") {turns++; if (turns === targetTurn) mutate("turn_start");}
   if (turns === targetTurn && event.message?.role === "user" && event.message.timestamp >= 10) mutate(event.type);
   events.push({type: event.type, ...(event.message ? {message: message(event.message)} : {}), ...(event.messages ? {messages: values(event.messages)} : {})});
   if (event.type === "agent_end") result = event.messages;
  };
  const next = () => {mutate("next"); hooks.push({type: "next", selected: values(selected), retained: values(retained)}); return subject === "prepared" ? {messages: selected} : undefined;};
  const config = {model, convertToLlm: (m: any[]) => m,
   getSteeringMessages: () => {steering++; if (steering === 3) mutate("poll"); const output = subject === "initial" && steering === 1 || subject === "steering" && steering === 2 ? selected : []; hooks.push({type: "steering", number: steering, messages: values(output)}); return output;},
   getFollowUpMessages: () => {followup++; const output = subject === "followup" && followup === 1 ? selected : []; hooks.push({type: "followup", number: followup, messages: values(output)}); return output;},
   prepareRequest: () => {if (++requested === targetTurn) mutate("request");},
   finishTurn: () => ({action: ++finished === 1 ? "continue" : "end"}), prepareNextTurn: next,
  };
  const stream = async (_model: any, context: any) => {requests.push(values(context.messages)); return response(requests.length);};
  const initial = {messages: [user(1)], tools: []};
  if (mode === "run") result = await runAgentLoop([user(2)], initial, config, emit, undefined, stream);
  if (mode === "continue") result = await runAgentLoopContinue(initial, config, emit, undefined, stream);
  if (mode === "agent") {
   agent = new Agent({initialState: {model, ...initial}, ...config, prepareNextTurn: undefined, prepareNextTurnWithContext: next, streamFn: stream} as any);
   agent.subscribe(emit); await agent.prompt(user(2));
  }
  mutate("settled");
  cases.push({input, expected: {events, requests, hooks, result: values(result), selected: values(selected), retained: values(retained), same: selected === retained, edited, ...(agent ? {state: values(agent.state.messages)} : {})}});
 }
}
const output = `{"upstreamCommit":${JSON.stringify(manifest.commit)},"cases":[\n${cases.map(value => JSON.stringify(value)).join(",\n")}\n]}\n`;
const destination = new URL("pi-input-lists.json", import.meta.url);
if (process.argv.includes("--check")) {if (readFileSync(destination, "utf8") !== output) throw new Error("Pi input-list fixtures differ");}
else writeFileSync(destination, output);
console.log(`Verified ${cases.length} input-list cases against Pi ${manifest.commit}`);
