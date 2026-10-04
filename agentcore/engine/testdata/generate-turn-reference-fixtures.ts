// Observe the same completed-turn object through hooks and retained references.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {runAgentLoop, runAgentLoopContinue} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const model = {id: "test", api: "test", provider: "test"};
const user = (timestamp: number) => ({role: "user", content: "go", timestamp});
const small = (m: any) => m == null ? null : {role: m.role, timestamp: m.timestamp};
const list = (messages: any) => messages == null ? null : messages.map(small);
const assistant = (n: number, stop: string) => ({role: "assistant", content: n === 1 && stop === "normal" ? [{type: "toolCall", id: "call", name: "echo", arguments: {}}] : [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: stop === "normal" ? "stop" : stop, timestamp: n + 1});
const cases = [];
for (const mode of ["run", "continue", "agent"]) for (const stop of ["normal", "error", "aborted"]) for (const subject of ["message", "toolResults", "context", "newMessages"]) {
 for (const phase of stop === "normal" ? ["finish", "turn_end", "steering", "next", "request", "settled"] : ["finish", "turn_end", "settled"]) {
  if (mode === "agent" && phase === "steering") continue;
  for (const action of ["replace", "clear", "local"]) for (const adopt of stop === "normal" && subject === "context" ? [false, true] : [false]) {
   const input = {mode, stop, subject, phase, action, adopt};
   const observations: any[] = [], requests: any[] = [], edits: any[] = [], refs: any[] = [];
   let first: any, original: any, completed = 0, requested = 0, responded = 0, edited = false, agent: any, result: any;
   const describe = (turn: any) => {
    let ref = refs.indexOf(turn); if (ref < 0) {ref = refs.length; refs.push(turn);}
    return {ref, message: small(turn.message), toolResults: list(turn.toolResults), context: turn.context == null ? null : list(turn.context.messages), newMessages: list(turn.newMessages), original: Object.fromEntries(Object.keys(original).map(key => [key, turn[key] === original[key]]))};
   };
   const mutate = (at: string) => {
    if (!first || edited || phase !== at) return;
    edited = true;
    let target = action === "local" ? {...first} : first;
    target[subject] = action === "clear" ? null : subject === "message" ? {...assistant(76, "normal"), content: [{type: "text", text: "replacement"}]} : subject === "context" ? {messages: [user(77)], tools: []} : subject === "toolResults" ? [{role: "toolResult", toolCallId: "replacement", toolName: "echo", content: [], isError: false, timestamp: 77}] : [user(77)];
    edits.push({at, same: target === first, turn: describe(target)});
   };
   const observe = (stage: string, turn: any = first) => observations.push({stage, turn: describe(turn), retained: describe(first)});
   const config = {model, convertToLlm: (messages: any) => messages,
    finishTurn: (turn: any) => {
     completed++;
     if (!first) {first = turn; original = {...turn}; mutate("finish");}
     observe(`finish:${completed}`, turn);
     return {action: completed === 1 ? "continue" : "end"};
    },
    prepareNextTurn: (turn: any) => {mutate("next"); observe("next", turn); return adopt ? {context: turn.context} : undefined;},
    prepareRequest: () => {if (requested++ > 0) {mutate("request"); observe("request");}},
    getSteeringMessages: () => {if (first) mutate("steering"); return [];},
   };
   const emit = (event: any) => {
    if (event.type === "turn_end") {
     if (completed === 1) mutate("turn_end");
     observe(`turn_end:${completed}`);
     observations.at(-1).event = {message: small(event.message), toolResults: list(event.toolResults)};
    }
    if (event.type === "agent_end") {observe("agent_end"); observations.at(-1).event = {messages: list(event.messages)}; result = event.messages;}
   };
   const stream = async (_model: any, context: any) => {
    requests.push(list(context.messages));
    const message = assistant(++responded, stop), s = new AssistantMessageEventStream();
    if (stop === "normal") s.push({type: "done", reason: "stop", message});
    else s.push({type: "error", reason: stop, error: message});
    return s;
   };
   const tools = [{name: "echo", label: "echo", description: "", parameters: {type: "object"}, execute: async () => ({content: [{type: "text", text: "ok"}], details: {}})}];
   if (mode === "run") result = await runAgentLoop([user(1)], {messages: [], tools}, config, emit, undefined, stream);
   if (mode === "continue") result = await runAgentLoopContinue({messages: [user(1)], tools}, config, emit, undefined, stream);
   if (mode === "agent") {
    agent = new Agent({initialState: {model, tools}, ...config, prepareNextTurn: undefined, prepareNextTurnWithContext: config.prepareNextTurn, streamFn: stream} as any);
    agent.subscribe(emit); await agent.prompt(user(1));
   }
   mutate("settled"); observe("settled");
   cases.push({input, expected: {requests, observations, edits, result: list(result), retained: refs.map(describe), ...(agent ? {state: list(agent.state.messages)} : {})}});
  }
 }
}
const output = JSON.stringify({upstreamCommit: manifest.commit, cases}, null, 2) + "\n";
const destination = new URL("pi-turn-references.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi turn-reference fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} turn-reference cases against Pi ${manifest.commit}`);
