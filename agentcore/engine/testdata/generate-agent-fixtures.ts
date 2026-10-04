import { readFileSync, writeFileSync } from "node:fs";
import { root, aiRoot, manifest } from "./oracle.ts";

const { Agent } = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const { AssistantMessageEventStream } = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
const now = 1700000000123;
Date.now = () => now;
const clone = (value: any) => JSON.parse(JSON.stringify(value));
const model = { id: "model", api: "api", provider: "provider", input: ["text"] };
const text = (text: string) => ({ type: "text", text });
const user = (content: string) => ({ role: "user", content, timestamp: now });
const system = (content: string, extra = {}) => ({ role: "system", content, timestamp: now, ...extra });
const tool = (name: string) => ({ name, label: name, description: `${name} tool`, parameters: { type: "object" } });
const usage = { input: 1, output: 2, cacheRead: 0, cacheWrite: 0, totalTokens: 3, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const assistant = (content: any[] = [text("done")], stopReason = "stop", extra = {}) => ({ role: "assistant", content, api: "api", provider: "provider", model: "model", usage, stopReason, timestamp: now, ...extra });
const response = (message = assistant(), extra = {}) => ({ message, ...extra });
const prompt = (input: any = "hello", images?: any[]) => ({ op: "prompt", input, ...(images ? { images } : {}) });
const steer = (content: string) => ({ op: "steer", message: user(content) });
const follow = (content: string) => ({ op: "followUp", message: user(content) });
const cases: any[] = [
  { name: "sparse initial history preserves field absence", initial: { messages: [{ role: "system", content: "policy", toolsAdded: [{ name: "read", parameters: { type: "object" } }] }, { role: "user", content: "question" }, { role: "assistant", content: [{ type: "text", text: "earlier" }] }, { role: "toolResult", toolCallId: "read", toolName: "read", content: [] }] }, actions: [] },
  { name: "nullable initial metadata retains original shape", initial: { messages: [{ role: "user", content: "question", timestamp: null }, { role: "assistant", content: [], model: null, usage: null, responseId: null }] }, actions: [] },
  { name: "replaced sparse history survives a prompt", actions: [{ op: "messages", messages: [{ role: "user", content: "earlier" }, { role: "assistant", content: [{ type: "text", text: "retained" }] }] }, prompt("next")], responses: [response()] },
  { name: "default state", actions: [] },
  { name: "initial system and tools", initial: { systemPrompt: "helpful", tools: [tool("first")], thinkingLevel: "low", model }, actions: [] },
  { name: "existing leading system wins", initial: { systemPrompt: "ignored", tools: [tool("first")], messages: [system("existing"), user("earlier")] }, actions: [] },
  { name: "empty text and images", actions: [prompt("", [{ type: "image", data: "YQ==", mimeType: "image/png" }])], responses: [response()] },
  { name: "single message prompt", actions: [prompt({ ...user("single"), host: { opaque: true } })], responses: [response()] },
  { name: "batch and default custom filtering", actions: [prompt([system("instruction"), { role: "custom", content: "display-only", timestamp: now, host: "opaque" }, user("go")])], responses: [response()] },
  { name: "custom converter is forwarded", convertCustom: true, actions: [prompt({ role: "custom", content: "custom", timestamp: now })], responses: [response()] },
  { name: "partial response state", actions: [prompt()], responses: [response(assistant(), { partial: true })] },
  { name: "tool pending lifecycle", initial: { tools: [tool("first")] }, actions: [prompt()], responses: [response(assistant([{ type: "toolCall", id: "one", name: "first", arguments: {} }], "toolUse")), response()] },
  { name: "mutators between prompts", actions: [prompt("first"), { op: "model", model }, { op: "thinking", value: "high" }, { op: "tools", tools: [tool("new")] }, prompt("second")], responses: [response(), response()] },
  { name: "initial queue preview and clear", actions: [steer("s1"), steer("s2"), follow("f1"), { op: "steeringMode", value: "all" }, { op: "clearSteering" }, follow("f2"), { op: "followUpMode", value: "all" }, { op: "clearFollowUp" }] },
  { name: "clear all queues", actions: [steer("s"), follow("f"), { op: "clearAll" }] },
  { name: "one-at-a-time queue scheduling", actions: [steer("s1"), steer("s2"), follow("f"), prompt()], responses: [response(), response(), response()] },
  { name: "all queue scheduling", options: { steeringMode: "all", followUpMode: "all" }, actions: [steer("s1"), steer("s2"), follow("f1"), follow("f2"), prompt()], responses: [response(), response()] },
  { name: "assistant continuation selects only one steering initially", initial: { messages: [assistant()] }, actions: [steer("s1"), steer("s2"), { op: "continue" }], responses: [response(), response()] },
  { name: "assistant continuation follows queued follow-up", initial: { messages: [assistant()] }, actions: [follow("f"), { op: "continue" }], responses: [response()] },
  { name: "non-assistant continuation keeps queue priority", initial: { messages: [user("earlier")] }, actions: [follow("f"), steer("s"), { op: "continue" }], responses: [response(), response()] },
  { name: "assistant continuation without queues rejected", initial: { messages: [assistant()] }, actions: [{ op: "continue" }] },
  { name: "empty continuation rejected", actions: [{ op: "continue" }] },
  { name: "system-only continuation rejected", initial: { systemPrompt: "baseline" }, actions: [{ op: "continue" }] },
  { name: "reset restores current system baseline", initial: { model, thinkingLevel: "high", tools: [tool("executable")], messages: [system("base", { sections: { a: "old", b: "keep" }, toolsAdded: [tool("old")] }), user("old user"), system("append", { sections: { a: "new" }, toolsRemoved: [{ name: "old" }], toolsAdded: [tool("declared")] }), assistant()] }, actions: [steer("s"), follow("f"), { op: "reset" }] },
  { name: "reset empty transcript", actions: [steer("s"), { op: "reset" }] },
  { name: "thrown provider failure emits full lifecycle", initial: { model }, actions: [prompt()], responses: [{ failure: "provider threw" }] },
  { name: "provider error recorded without synthetic duplicate", actions: [prompt()], responses: [response(assistant([], "error", { errorMessage: "provider error" }))] },
  { name: "error state cleared by next run", initial: { model }, actions: [prompt("fails"), prompt("works")], responses: [{ failure: "first failure" }, response()] },
  { name: "convert failure emits full lifecycle", convertFailure: "convert failed", actions: [prompt()] },
  { name: "subscriber failure recovered once", reactions: [{ event: "message_start", actions: [{ op: "throw", value: "listener failed" }] }], actions: [prompt()] },
  { name: "persistent subscriber failure rejects and settles", reactions: [{ event: "message_start", always: true, actions: [{ op: "throw", value: "persistent listener failure" }] }], actions: [prompt()] },
  { name: "busy admission is rejected through agent_end", reactions: [{ event: "agent_end", actions: [prompt("busy"), { op: "continue" }, { op: "reset" }] }], actions: [prompt()], responses: [response()] },
  { name: "abort signal reaches subscriber and failure state", initial: { model }, reactions: [{ event: "agent_start", actions: [{ op: "abort" }] }], actions: [prompt()], responses: [{ failure: "aborted provider" }] },
  { name: "finish end retains queued messages", decisions: ["end"], reactions: [{ event: "message_end", role: "assistant", actions: [steer("s"), follow("f")] }], actions: [prompt()], responses: [response()] },
  { name: "legacy preparation callback receives signal", legacyPrepare: true, nextUpdates: [{ messages: [user("prepared")], thinkingLevel: "high" }], decisions: ["continue", "end"], actions: [prompt()], responses: [response(), response()] },
  { name: "context preparation takes precedence", legacyPrepare: true, contextPrepare: true, nextUpdates: [{ model, thinkingLevel: "low" }], decisions: ["continue", "end"], actions: [prompt()], responses: [response(), response()] },
  { name: "request preparation does not overwrite persistent model", initial: { model }, requestUpdates: [{ model: { ...model, id: "request-only" }, thinkingLevel: "high" }], actions: [prompt()], responses: [response()] },
  { name: "provider controls forwarded", initial: { thinkingLevel: "medium" }, options: { sessionId: "session-1", thinkingBudgets: { medium: 2000 }, transport: "sse", maxRetryDelayMs: 3000 }, actions: [prompt()], responses: [response()] },
];

for (const terminate of [false, true]) cases.push({
  name: `tool end subscriber mutation/${terminate ? "terminate" : "continue"}`,
  initial: {tools: [tool("first")]}, actions: [prompt()],
  toolEndContent: "redacted", toolEndTerminate: terminate,
  responses: [response(assistant([{type: "toolCall", id: "one", name: "first", arguments: {}}], "toolUse")), response()],
});

for (const role of ["user", "assistant", "toolResult"]) {
  for (const at of ["message_start", "message_end", "agent_end"]) cases.push({
    name: `message subscriber mutation/${role}/${at}`,
    initial: {tools: role === "toolResult" ? [tool("first")] : []},
    actions: [prompt(), prompt("next")],
    messageMutationRole: role, messageMutationAt: at,
    responses: role === "toolResult"
      ? [response(assistant([{type: "toolCall", id: "one", name: "first", arguments: {}}], "toolUse")), response(), response()]
      : [response(), response()],
  });
}
for (const at of ["message_start", "message_end", "turn_end"]) cases.push({
  name: `failure message subscriber mutation/${at}`, initial: {model},
  actions: [prompt(), prompt("next")], responses: [{failure: "provider failed"}, response()],
  messageMutationRole: "assistant", messageMutationAt: at,
});
const results = [];
for (const input of cases) {
  const events: any[] = [], requests: any[] = [], checks: any[] = [], hooks: any[] = [];
  let responseIndex = 0, finishIndex = 0, prepareIndex = 0, requestIndex = 0;
  const fired = new Set<number>();
  const makeTools = (specs: any[] = []) => specs.map(spec => ({ ...clone(spec), execute: async () => ({ content: [text(spec.name)], details: {} }) }));
  let agent: any;
  const config: any = { ...input.options, initialState: { ...clone(input.initial ?? {}), tools: makeTools(input.initial?.tools) },
    streamFn: async (requestedModel: any, context: any, options: any) => {
      const { signal: _signal, ...forwarded } = options;
      requests.push(clone({ model: requestedModel, context, options: forwarded, signalMatches: options.signal === agent.signal, aborted: options.signal.aborted }));
      const scripted = input.responses?.[responseIndex++];
      if (!scripted) throw new Error("oracle script exhausted");
      if (scripted.failure) throw new Error(scripted.failure);
      const final = clone(scripted.message);
      const stream = new AssistantMessageEventStream();
      if (scripted.partial) {
        stream.push({ type: "start", partial: { ...clone(final), content: [] } });
        stream.push({ type: "text_delta", contentIndex: 0, delta: "done", partial: clone(final) });
      }
      if (["error", "aborted"].includes(final.stopReason)) stream.push({ type: "error", reason: final.stopReason, error: final });
      else stream.push({ type: "done", reason: final.stopReason, message: final });
      return stream;
    },
  };
  if (input.convertCustom || input.convertFailure) config.convertToLlm = (messages: any[]) => {
    if (input.convertFailure) throw new Error(input.convertFailure);
    return messages.map(message => message.role === "custom" ? { ...message, role: "user" } : message);
  };
  if (input.decisions) config.finishTurn = (turn: any, signal: any) => {
    hooks.push({ hook: "finish", tools: turn.toolResults.map((m: any) => m.toolCallId), signalMatches: signal === agent.signal });
    const action = input.decisions[finishIndex++]; return action ? { action } : undefined;
  };
  if (input.legacyPrepare) config.prepareNextTurn = (signal: any) => {
    hooks.push({ hook: "legacyPrepare", signalMatches: signal === agent.signal });
    return input.nextUpdates?.[prepareIndex++];
  };
  if (input.contextPrepare) config.prepareNextTurnWithContext = (turn: any, signal: any) => {
    hooks.push({ hook: "contextPrepare", messageRole: turn.message.role, newMessages: turn.newMessages.length, signalMatches: signal === agent.signal });
    return input.nextUpdates?.[prepareIndex++];
  };
  if (input.requestUpdates) config.prepareRequest = (_request: any, signal: any) => {
    hooks.push({ hook: "request", signalMatches: signal === agent.signal });
    return input.requestUpdates[requestIndex++];
  };
  agent = new Agent(config);
  const state = () => clone({ ...agent.state, pendingToolCalls: [...agent.state.pendingToolCalls] });
  const checkpoint = (op: string, error?: string) => checks.push(clone({ op, ...(error ? { error } : {}), state: state(), queued: agent.hasQueuedMessages(), peek: agent.peekQueuedMessages(), steeringMode: agent.steeringMode, followUpMode: agent.followUpMode, signalPresent: agent.signal !== undefined }));
  async function act(action: any, nested = false) {
    let error: string | undefined;
    try {
      switch (action.op) {
        case "prompt": await agent.prompt(clone(action.input), clone(action.images ?? [])); break;
        case "continue": await agent.continue(); break;
        case "reset": agent.reset(); break;
        case "abort": agent.abort(); break;
        case "steer": agent.steer(clone(action.message)); break;
        case "followUp": agent.followUp(clone(action.message)); break;
        case "steeringMode": agent.steeringMode = action.value; break;
        case "followUpMode": agent.followUpMode = action.value; break;
        case "clearSteering": agent.clearSteeringQueue(); break;
        case "clearFollowUp": agent.clearFollowUpQueue(); break;
        case "clearAll": agent.clearAllQueues(); break;
        case "model": agent.state.model = clone(action.model); break;
        case "thinking": agent.state.thinkingLevel = action.value; break;
        case "tools": agent.state.tools = makeTools(action.tools); break;
        case "messages": agent.state.messages = clone(action.messages); break;
        case "throw": throw new Error(action.value);
        default: throw new Error(`unknown action ${action.op}`);
      }
    } catch (failure) { error = (failure as Error).message; if (action.op === "throw") throw failure; }
    checkpoint((nested ? "listener:" : "") + action.op, error);
  }
  let retainedMessage: any, messageMutated = false;
  agent.subscribe(async (event: any, signal: any) => {
    events.push(clone({ event, state: state(), signalMatches: signal === agent.signal, aborted: signal.aborted }));
    if (event.type === "message_end" && event.message.role === input.messageMutationRole && !retainedMessage) retainedMessage = event.message;
    if (!messageMutated && event.type === input.messageMutationAt && (event.type === "agent_end" || event.message?.role === input.messageMutationRole)) {
      const message = event.type === "agent_end" ? retainedMessage : event.message;
      message.content = message.role === "user" ? "callback revision" : [text("callback revision")];
      message.timestamp = now + 9;
      messageMutated = true;
      checkpoint("message mutation");
    }
    if (event.type === "tool_execution_end" && input.toolEndContent !== undefined) {
      event.result.content = [text(input.toolEndContent)];
      event.result.terminate = input.toolEndTerminate;
    }
    for (let i = 0; i < (input.reactions?.length ?? 0); i++) {
      const reaction = input.reactions[i];
      if (event.type !== reaction.event || (reaction.role && event.message?.role !== reaction.role) || (fired.has(i) && !reaction.always)) continue;
      fired.add(i);
      for (const action of reaction.actions) await act(action, true);
    }
  });
  checkpoint("initial");
  for (const action of input.actions) await act(action);
  results.push({ input, expected: { events, requests, checks, hooks } });
}
const destination = new URL("pi-agent.json", import.meta.url);
const serialized = `{\n  "upstreamCommit": ${JSON.stringify(manifest.commit)},\n  "cases": [\n${results.map(result => "    " + JSON.stringify(result)).join(",\n")}\n  ]\n}\n`;
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Pi Agent fixtures changed; regenerate and review");
} else writeFileSync(destination, serialized);
console.log(`Verified ${results.length} stateful Agent fixtures against Pi ${manifest.commit}`);
