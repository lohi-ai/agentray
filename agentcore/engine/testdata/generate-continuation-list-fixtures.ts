import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {runAgentLoopContinue, agentLoopContinue} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const message = (role: string, timestamp = 1): any => ({role, content: "go", timestamp});
const shape = (m: any) => m == null ? null : {role: m.role, timestamp: m.timestamp, error: m.errorMessage ?? null};
const values = (list: any[]) => Array.from(list, shape);
const summarize = (list: any[]) => ({length: list.length, keys: Object.keys(list), values: values(list)});
const patterns = ["empty", "holes", "user_hole", "user_null", "hole_system", "hole_assistant", "hole_user", "system_hole_user", "system_null_user", "null_user", "user_null_user", "system", "assistant", "user", "system_null_assistant", "user_hole_assistant", "null_hole"];
const history = (pattern: string) => {
 if (pattern === "empty") return [];
 if (pattern === "holes") return new Array(2);
 const list = pattern.split("_").map((role, index) => role === "hole" || role === "null" ? null : message(role, index + 1));
 pattern.split("_").forEach((role, index) => {if (role === "hole") delete list[index];});
 return list;
};
const cases = [];
for (const mode of ["agent", "loop"]) for (const pattern of patterns) for (const queue of ["none", "steering", "followup", "both"]) for (const edit of ["none", "start_fill", "constructor"]) {
 const input = {mode, pattern, queue, edit};
 let selected = history(pattern), agent: any;
 const events: any[] = [], requests: any[] = [];
 let repaired = false, steering = 0, followup = 0, ended: any[] | undefined;
 const emit = (event: any) => {
  if (event.type === "agent_start" && edit === "start_fill" && !repaired) {
   repaired = true;
   for (let i = 0; i < selected.length; i++) if (selected[i] == null) selected[i] = message("user", 70 + i);
  }
  events.push({type: event.type, ...(event.message ? {message: shape(event.message)} : {}), ...(event.messages ? {messages: values(event.messages)} : {})});
  if (event.type === "agent_end") ended = event.messages;
 };
 const stream = async (_model: any, context: any) => {
  requests.push(values(context.messages));
  const output = {role: "assistant", content: [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "stop", timestamp: 20};
  const stream = new AssistantMessageEventStream(); stream.push({type: "done", reason: "stop", message: output}); return stream;
 };
 const config = {model, finishTurn: () => ({action: "end"}), convertToLlm: (messages: any[]) => messages,
  getSteeringMessages: () => ++steering === 1 && (queue === "steering" || queue === "both") ? [message("user", 90)] : [],
  getFollowUpMessages: () => ++followup === 1 && (queue === "followup" || queue === "both") ? [message("user", 91)] : [],
 };
 if (mode === "agent") {
  agent = new Agent({initialState: {model}, ...config, streamFn: stream} as any);
  agent.state.messages = selected; selected = agent.state.messages;
  if (queue === "steering" || queue === "both") agent.steer(message("user", 90));
  if (queue === "followup" || queue === "both") agent.followUp(message("user", 91));
  agent.subscribe(emit);
 }
 if (edit === "constructor") Object.defineProperty(selected, "constructor", {value: null, enumerable: true, configurable: true});
 let error: string | null = null;
 try {if (agent) await agent.continue(); else await runAgentLoopContinue({messages: selected, tools: []}, config, emit, undefined, stream);}
 catch (failure) {error = (failure as Error).message;}
 const observe = () => ({events: [...events], requests: [...requests], history: summarize(selected), result: ended ? values(ended) : null, repaired, ...(agent ? {streaming: agent.state.isStreaming, error: agent.state.errorMessage ?? null, signal: agent.signal !== undefined, queued: agent.hasQueuedMessages(), peek: values(agent.peekQueuedMessages())} : {steering, followup})});
 const first = {failure: error, ...observe()};
 // Every rejected admission/run must leave the wrapper usable and its queues intact.
 let recovered: any = null;
 if (agent) {agent.state.messages = [message("user", 30)]; selected = agent.state.messages; await agent.continue(); await agent.waitForIdle(); recovered = observe();}
 cases.push({input, expected: {first, recovered}});
}
const wrappers = [];
for (const pattern of ["empty", "holes", "user_hole", "user_null", "hole_assistant", "assistant"]) {
 let error: string | null = null;
 try {agentLoopContinue({messages: history(pattern), tools: []}, {model, convertToLlm: (messages: any[]) => messages} as any, undefined, async () => {throw new Error("unexpected provider call");});}
 catch (failure) {error = (failure as Error).message;}
 wrappers.push({pattern, error});
}
const reads = [];
for (const pattern of patterns) for (const operation of ["prompt", "json", "reset"]) for (const constructor of [false, true]) {
 const agent = new Agent({initialState: {model}, streamFn: async () => {throw new Error("unexpected provider call");}} as any);
 agent.state.messages = history(pattern);
 if (constructor) Object.defineProperty(agent.state.messages, "constructor", {value: null, enumerable: true, configurable: true});
 agent.steer(message("user", 90)); agent.followUp(message("user", 91));
 let value: any = null, error: string | null = null;
 try {
  if (operation === "prompt") value = agent.state.systemPrompt;
  if (operation === "json") value = JSON.parse(JSON.stringify(agent.state));
  if (operation === "reset") agent.reset();
 } catch (failure) {error = (failure as Error).message;}
 reads.push({input: {pattern, operation, constructor}, expected: {value, error, history: summarize(agent.state.messages), queued: agent.hasQueuedMessages(), peek: values(agent.peekQueuedMessages())}});
}
const output = `{"upstreamCommit":${JSON.stringify(manifest.commit)},"cases":[\n${cases.map(value => JSON.stringify(value)).join(",\n")}\n],"wrappers":${JSON.stringify(wrappers)},"reads":${JSON.stringify(reads)}}\n`;
const destination = new URL("pi-continuation-lists.json", import.meta.url);
if (process.argv.includes("--check")) {if (readFileSync(destination, "utf8") !== output) throw new Error("Pi continuation-list fixtures differ");}
else writeFileSync(destination, output);
console.log(`Verified ${cases.length} continuation-list, ${wrappers.length} synchronous-wrapper and ${reads.length} history-read cases against Pi ${manifest.commit}`);
