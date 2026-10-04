import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {runAgentLoop, runAgentLoopContinue} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const user = (timestamp: number) => ({role: "user", content: "go", timestamp});
const messages = (list: any[]) => list.map(m => ({role: m.role, timestamp: m.timestamp, ...(m.role === "toolResult" ? {id: m.toolCallId, isError: m.isError} : {})}));
const toolValues = (list: any[]) => list.map(t => ({name: t.name, label: t.label}));
const cases = [];
for (const mode of ["run", "continue", "agent"]) for (const subject of ["messages", "tools"]) for (const phase of ["request", "transform", "convert", "key", "start", "update", "end", "before", "finish", "next", "request2", "settled"]) for (const action of ["append", "replace", "shrink", "rebind_same", "rebind_new", "detach_append", "nested"]) {
 const input = {mode, subject, phase, action};
 const observations: any[] = [], requests: any[] = [], executed: any[] = [], refs: any[] = [];
 const tool = (name: string) => ({name, label: name, description: "", parameters: {type: "object"}, execute: async (id: string) => {executed.push({executor: name, id}); return {content: [{type: "text", text: "ok"}], details: {}};}});
 const initial = {messages: [user(1)], tools: [tool("echo")]};
 let current: any, retained: any[], edited = false, requested = 0, responded = 0, finished = 0, result: any[], agent: any;
 const ref = (value: any) => {let id = refs.indexOf(value); if (id < 0) {id = refs.length; refs.push(value);} return id;};
 const values = (list: any[], kind = subject) => kind === "messages" ? messages(list) : toolValues(list);
 const mutate = (at: string) => {
  if (edited || phase !== at) return;
  edited = true;
  const replacement = () => subject === "messages" ? user(77) : tool("extra");
  if (action === "append") retained.push(replacement());
  if (action === "replace") retained[0] = replacement();
  if (action === "shrink") retained.length = 0;
  if (action === "rebind_same") current[subject] = retained;
  if (action === "rebind_new") current[subject] = [replacement()];
  if (action === "detach_append") {current[subject] = retained.slice(); retained.push(replacement());}
  if (action === "nested") {if (subject === "messages") retained[0].timestamp = 88; else retained[0].name = "extra";}
 };
 const observe = (stage: string, parameter?: any[]) => observations.push({stage, messageRef: ref(current.messages), toolRef: ref(current.tools), messages: messages(current.messages), messageKeys: Object.keys(current.messages), negativeLast: Object.hasOwn(current.messages, "-1") ? messages([current.messages[-1]]) : [], tools: toolValues(current.tools), retainedRef: ref(retained), retained: values(retained), same: retained === current[subject], ...(parameter ? {parameterRef: ref(parameter), parameter: messages(parameter), parameterIsCurrent: parameter === current.messages} : {})});
 const config = {model, toolExecution: "sequential",
  prepareRequest: (request: any) => {current = request.context; if (!retained) retained = current[subject]; mutate(requested++ === 0 ? "request" : "request2"); observe(`request:${requested}`);},
  transformContext: (parameter: any[]) => {mutate("transform"); observe("transform", parameter); return parameter;},
  convertToLlm: (parameter: any[]) => {mutate("convert"); observe("convert", parameter); return parameter;},
  getApiKey: () => {mutate("key"); observe("key"); return "key";},
  beforeToolCall: (call: any) => {mutate("before"); observe(`before:${call.toolCall.id}`);},
  finishTurn: () => {if (++finished === 1) mutate("finish"); observe(`finish:${finished}`); return {action: finished === 1 ? "continue" : "end"};},
  prepareNextTurn: () => {mutate("next"); observe("next");},
 };
 const stream = async (_model: any, context: any) => {
  requests.push(messages(context.messages));
  const number = ++responded;
  const message = {role: "assistant", content: number === 1 ? [{type: "toolCall", id: "first", name: "echo", arguments: {}}, {type: "toolCall", id: "second", name: "extra", arguments: {}}] : [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "stop", timestamp: number + 1};
  const s = new AssistantMessageEventStream();
  s.push({type: "start", partial: message});
  s.push({type: "text_delta", contentIndex: 0, delta: "", partial: message});
  s.push({type: "done", reason: "stop", message});
  return s;
 };
 const emit = (event: any) => {
  if (event.message?.role === "assistant") {
   const at = ({message_start: "start", message_update: "update", message_end: "end"} as any)[event.type];
   if (at) {mutate(at); observe(at);}
  }
  if (event.type === "agent_end") {result = event.messages; observe("agent_end");}
 };
 if (mode === "run") result = await runAgentLoop([user(10)], initial, config, emit, undefined, stream);
 if (mode === "continue") result = await runAgentLoopContinue(initial, config, emit, undefined, stream);
 if (mode === "agent") {
  agent = new Agent({initialState: {model, ...initial}, ...config, prepareNextTurn: undefined, prepareNextTurnWithContext: config.prepareNextTurn, streamFn: stream} as any);
  agent.subscribe(emit); await agent.prompt(user(10));
 }
 mutate("settled"); observe("settled");
 cases.push({input, expected: {observations, requests, executed, result: messages(result!), initial: {messages: messages(initial.messages), tools: toolValues(initial.tools), sameMessages: initial.messages === current.messages, sameTools: initial.tools === current.tools}, ...(agent ? {state: {messages: messages(agent.state.messages), tools: toolValues(agent.state.tools), sameMessages: agent.state.messages === current.messages, sameTools: agent.state.tools === current.tools}} : {})}});
}
const defaults = [];
for (const variant of ["identity", "sparse", "mixed", "grow_empty", "key_append", "key_nested"]) {
 let selected: any[];
 const requests: any[] = [];
 const agent = new Agent({initialState: {model},
  transformContext: (input: any[]) => {
   if (variant === "identity") return selected = input;
   selected = []; selected.length = 4;
   if (variant !== "grow_empty") selected[1] = user(77);
   if (variant === "mixed") selected[3] = {role: "custom", content: "custom", timestamp: 99};
   return selected;
  },
  getApiKey: () => {if (variant === "key_append") selected.push(user(88)); if (variant === "key_nested") selected[1].timestamp = 88; return "key";},
  streamFn: async (_model: any, context: any) => {
   requests.push(messages(context.messages));
   const message = {role: "assistant", content: [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "stop", timestamp: 2};
   const stream = new AssistantMessageEventStream(); stream.push({type: "done", reason: "stop", message}); return stream;
  },
 } as any);
 await agent.prompt(user(1));
 defaults.push({variant, expected: {requests, selected: messages(selected!), keys: Object.keys(selected!), state: messages(agent.state.messages)}});
}
// One generated case per line avoids repeating megabytes of indentation.
const output = `{"upstreamCommit":${JSON.stringify(manifest.commit)},"cases":[\n${cases.map(value => JSON.stringify(value)).join(",\n")}\n],"defaults":${JSON.stringify(defaults)}}\n`;
const destination = new URL("pi-context-lists.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi context-list fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} context-list and ${defaults.length} default-filter cases against Pi ${manifest.commit}`);
