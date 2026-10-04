// State collection and streaming-message oracle from the unchanged Pi Agent.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const clone = (v: any) => JSON.parse(JSON.stringify(v));
const model = {id: "test", api: "test", provider: "test"};
const user = (text: string) => ({role: "user", content: text, timestamp: 1});
const tool = (name: string) => ({name, label: name, description: name, parameters: {type: "object"}, execute: async () => ({content: [], details: {}})});
const assistant = () => ({role: "assistant", content: [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", stopReason: "stop", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, timestamp: 2});
const stream = async (partial = false) => {
 const s = new AssistantMessageEventStream(), message = assistant();
 if (partial) {s.push({type: "start", partial: message}); s.push({type: "text_delta", contentIndex: 0, delta: "done", partial: message});}
 s.push({type: "done", reason: "stop", message});
 return s;
};
const cases = [];
for (const subject of ["messages", "tools"]) for (const phase of ["plain", "caller_replace", "entry_replace", "append", "nested_edit", "delete", "shrink", "grow", "assign_empty", "assign_new", "assign_same", "assign_sparse", "retained_after_assign", "reset"]) {
 const input = {subject, phase};
 const initialMessages = [{role: "system", content: "policy", timestamp: 0}, user("original")];
 const initialTools = [tool("original")];
 const initial: any = subject === "messages" ? initialMessages : initialTools;
 const index = subject === "messages" ? 1 : 0;
 const entry = initial[index];
 const replacement = () => subject === "messages" ? user("replacement") : tool("replacement");
 const agent = new Agent({initialState: {model, messages: initialMessages, tools: initialTools} as any, streamFn: (() => stream()) as any});
 const retained: any[] = agent.state[subject];
 const initialSame = retained === initial;
 if (phase === "caller_replace") initial[index] = replacement();
 if (phase === "entry_replace") retained[index] = replacement();
 if (phase === "append") retained.push(replacement());
 if (phase === "nested_edit") {if (subject === "messages") retained[index].timestamp = 77; else retained[index].name = "changed";}
 if (phase === "delete") delete retained[index];
 if (phase === "shrink") retained.length = 0;
 if (phase === "grow") retained.length += 3;
 if (phase === "assign_empty") agent.state[subject] = [];
 if (phase === "assign_new" || phase === "retained_after_assign") agent.state[subject] = [replacement()];
 if (phase === "assign_same") agent.state[subject] = retained;
 if (phase === "assign_sparse") {delete retained[index]; agent.state[subject] = retained;}
 if (phase === "retained_after_assign") retained.push(replacement());
 if (phase === "reset") agent.reset();
 const current: any[] = agent.state[subject];
 cases.push({input, expected: {initialSame, sameList: current === retained, sameEntry: retained[index] === entry, retained: clone(retained), current: clone(current), initial: clone(initial), retainedKeys: Object.keys(retained).map(Number), currentKeys: Object.keys(current).map(Number)}});
}
const runs = [];
for (const subject of ["messages", "tools"]) for (const phase of ["before", "agent_start", "user_start", "user_end", "assistant_start", "assistant_update", "assistant_end", "after"]) for (const action of ["append", "replace", "nested"]) for (const partial of [false, true]) {
 if (!partial && phase === "assistant_update") continue;
 const input = {subject, phase, action, partial};
 const requests: any[] = [], observations: any[] = [];
 const agent = new Agent({initialState: {model, messages: [{role: "system", content: "policy", timestamp: 0}, user("original")], tools: [tool("original")]} as any, convertToLlm: (m: any) => m, streamFn: (async (_model: any, context: any) => {requests.push(clone(context)); return stream(partial);}) as any, finishTurn: () => ({action: "end"})});
 const retained = agent.state[subject];
 const index = subject === "messages" ? 1 : 0;
 const edit = () => {
  const value = subject === "messages" ? user("edited") : tool("edited");
  if (action === "append") retained.push(value);
  if (action === "replace") retained[index] = value;
  if (action === "nested") {if (subject === "messages") retained[index].timestamp = 77; else retained[index].name = "edited";}
 };
 if (phase === "before") edit();
 agent.subscribe((event: any) => {
  const at = event.type === "message_update" ? "assistant_update" : event.type === "agent_start" ? "agent_start" : event.type === "message_start" ? event.message.role === "user" ? "user_start" : "assistant_start" : event.type === "message_end" ? event.message.role === "user" ? "user_end" : "assistant_end" : "";
  if (phase === at) edit();
  if (event.type === "message_start" || event.type === "message_update") {
   const streaming = agent.state.streamingMessage;
   const same = streaming === event.message;
   streaming.timestamp = event.type === "message_update" ? 93 : event.message.role === "user" ? 91 : 92;
   observations.push({stage: at, same, eventTimestamp: event.message.timestamp});
  }
  if (event.type === "message_end") observations.push({stage: at, same: agent.state.messages.at(-1) === event.message});
 });
 await agent.prompt(user("prompt") as any);
 if (phase === "after") edit();
 runs.push({input, expected: {requests, observations, retained: clone(retained), current: clone(agent.state[subject]), sameList: retained === agent.state[subject], streamingCleared: agent.state.streamingMessage === undefined}});
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases, runs}, null, 2) + "\n";
const destination = new URL("pi-state-lists.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi state-list fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} state-list and ${runs.length} run cases against Pi ${manifest.commit}`);
