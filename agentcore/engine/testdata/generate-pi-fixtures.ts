// Development oracle. Production and go test execute only the native Go port.
import { readFileSync, writeFileSync } from "node:fs";
import { root, aiRoot, manifest } from "./oracle.ts";
const { runAgentLoop, runAgentLoopContinue } = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const { AssistantMessageEventStream } = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
const now = 1700000000123;
Date.now = () => now;
const model = { id: "test-model", provider: "test-provider", api: "test-api", input: ["text"] };
const usage = { input: 1, output: 2, cacheRead: 0, cacheWrite: 0, totalTokens: 3, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const text = (text: string) => ({ type: "text", text });
const user = (content: string) => ({ role: "user", content, timestamp: now });
const assistant = (content: any[] = [text("done")], stopReason = "stop") => ({ role: "assistant", content, timestamp: now, api: model.api, provider: model.provider, model: model.id, usage, stopReason });
const call = (id: string, name: string, args: any = {}) => ({ type: "toolCall", id, name, arguments: args });
const result = (value: string, extra = {}) => ({ content: [text(value)], details: {}, ...extra });
const echo = { name: "echo", description: "Echo", parameters: { type: "object", properties: { value: { type: "string" } }, required: ["value"] } };
const base = { prompts: [user("go")], messages: [], tools: [], responses: [assistant()] };
const cases: any[] = [
  { name: "simple prompt", ...base },
  { name: "continue from user", ...base, prompts: [], messages: [user("earlier")], resume: true },
  { name: "empty continuation rejected", ...base, resume: true },
  { name: "assistant continuation rejected", ...base, messages: [assistant()], resume: true },
  { name: "custom message converted once", ...base, prompts: [{ role: "custom", content: "custom prompt", timestamp: now, host: { id: 7 } }], convertCustom: true },
  { name: "transform request only", ...base, transformPrefix: user("request-only") },
  { name: "streamed response", ...base, streamPartial: true },
  { name: "result without terminal event", ...base, omitTerminal: true },
  { name: "provider error is hard exit", ...base, responses: [{ ...assistant([], "error"), errorMessage: "provider failed" }], decisions: ["continue"], steering: [[], [user("never drained")]], followUps: [[user("never followed")]] },
  { name: "provider abort is hard exit", ...base, responses: [{ ...assistant([], "aborted"), errorMessage: "aborted" }], decisions: ["continue"] },
  { name: "explicit context-only continuation", ...base, responses: [assistant(), assistant([text("second")])], decisions: ["continue", "end"] },
  { name: "steering satisfies explicit continuation", ...base, responses: [assistant(), assistant()], decisions: ["continue"], steering: [[], [user("steer")]] },
  { name: "follow-up satisfies explicit continuation", ...base, responses: [assistant(), assistant()], decisions: ["continue"], followUps: [[user("follow")], []] },
  { name: "end decision skips queue polls", ...base, decisions: ["end"], steering: [[], [user("ignored")]], followUps: [[user("ignored")]] },
  { name: "steering collected after preparation", ...base, responses: [assistant(), assistant()], decisions: ["continue", "end"], steering: [[], [], [user("during prepare")]], nextUpdates: [{ messages: [user("prepared")], model: { ...model, id: "next-model" }, thinkingLevel: "high" }] },
  { name: "already pending steering polled once", ...base, responses: [assistant(), assistant()], decisions: ["continue", "end"], steering: [[], [user("one")], [user("must stay queued")]], nextUpdates: [{ thinkingLevel: "off" }], reasoning: "high" },
  { name: "request preparation overrides first and later request", ...base, responses: [assistant(), assistant()], decisions: ["continue", "end"], requestUpdates: [{ model: { ...model, id: "request-model" }, thinkingLevel: "low" }, { thinkingLevel: "off" }], apiKeys: ["rotated", ""], apiKey: "fallback" },
  { name: "executable tools replace pending declarations", ...base, tools: [echo], prompts: [{ role: "system", content: "prompt", timestamp: now, toolsAdded: [{ ...echo, name: "stale" }] }, user("go")] },
  { name: "tool removal declared", ...base, messages: [{ role: "system", content: "prompt", timestamp: now, toolsAdded: [echo] }] },
  { name: "validated tool execution", ...base, tools: [echo], responses: [assistant([call("one", "echo", { value: true })], "toolUse"), assistant()] },
  { name: "optional null omitted", ...base, tools: [{ ...echo, parameters: { type: "object", properties: { value: { type: "string" }, optional: { type: "number" } }, required: ["value"] } }], responses: [assistant([call("one", "echo", { value: "x", optional: null })], "toolUse"), assistant()] },
  { name: "argument preparation retains raw trace", ...base, tools: [{ ...echo, prepare: { value: "prepared" } }], responses: [assistant([call("one", "echo", "raw")], "toolUse"), assistant()], recordHooks: true },
  { name: "unknown tool", ...base, responses: [assistant([call("one", "missing")], "toolUse"), assistant()] },
  { name: "invalid tool arguments", ...base, tools: [echo], responses: [assistant([call("one", "echo", { value: { nested: true } })], "toolUse"), assistant()] },
  { name: "required tool argument", ...base, tools: [echo], responses: [assistant([call("one", "echo", {})], "toolUse"), assistant()] },
  { name: "blocked terminating tool", ...base, tools: [{ ...echo, before: { block: true, terminate: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "tool failure still invokes after hook", ...base, tools: [{ ...echo, failure: "tool failed" }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse"), assistant()], recordHooks: true },
  { name: "tool result error flag retained with after override", ...base, tools: [{ ...echo, result: result("failed", { isError: true, terminate: true }), after: { isError: false } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after content drops stale structured content", ...base, tools: [{ ...echo, result: result("original", { structuredContent: { answer: 1 }, terminate: true }), after: { content: [] } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after full replacement and usage", ...base, tools: [{ ...echo, result: result("original"), after: { content: [text("replacement")], details: { replaced: true }, structuredContent: { answer: 2 }, usage, terminate: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after failure becomes tool error", ...base, tools: [{ ...echo, afterFailure: "after failed" }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse"), assistant()], recordHooks: true },
  { name: "partial and late updates", ...base, tools: [{ ...echo, updates: [result("partial")], lateUpdate: result("late"), result: result("final", { terminate: true }) }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")] },
  { name: "truncated calls never execute", ...base, tools: [echo], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" })], "length"), assistant()], recordHooks: true },
  { name: "sequential batch", ...base, mode: "sequential", tools: [echo], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" })], "toolUse"), assistant()], recordHooks: true },
  { name: "tool forces whole batch sequential", ...base, tools: [{ ...echo, executionMode: "sequential" }], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" })], "toolUse"), assistant()], recordHooks: true },
  { name: "mixed termination does not end batch", ...base, mode: "sequential", tools: [{ ...echo, result: result("done", { terminate: true }) }, { ...echo, name: "other", result: result("continue") }], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "other", { value: "y" })], "toolUse"), assistant()] },
  { name: "all terminating results end batch", ...base, mode: "sequential", tools: [{ ...echo, result: result("done", { terminate: true }) }], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" })], "toolUse")] },
  { name: "parallel end order differs from transcript order", ...base, waitForSecond: true, tools: [echo], responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" })], "toolUse"), assistant()] },
  { name: "missing result content normalized in transcript", ...base, tools: [{ ...echo, result: { details: {}, terminate: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")] },
  { name: "before hook failure skips execution and after", ...base, tools: [{ ...echo, beforeFailure: "before failed" }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse"), assistant()], recordHooks: true },
  { name: "sequential cancellation stops admission", ...base, mode: "sequential", cancelBefore: "two", tools: [echo], recordHooks: true, responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" }), call("three", "echo", { value: "z" })], "toolUse"), assistant([], "aborted")] },
  { name: "parallel cancellation prevents prepared executions", ...base, cancelBefore: "two", tools: [echo], recordHooks: true, responses: [assistant([call("one", "echo", { value: "x" }), call("two", "echo", { value: "y" }), call("three", "echo", { value: "z" })], "toolUse"), assistant([], "aborted")] },
  { name: "missing stream fails before queue polling", ...base, omitStream: true },
  { name: "sink failure stops before agent end", ...base, failEvent: "message_end" },
  { name: "argument key order and JSON string spelling", ...base, tools: [{ ...echo, parameters: { type: "object" } }], responses: [assistant([call("one", "echo", { z: "<>&\u2028\u2029", a: "\\u2028", nested: { z: 1, a: 2 }, "10": "ten", "2": "two" })], "toolUse"), assistant()] },
  { name: "null tool result fields retained in events", ...base, tools: [{ ...echo, result: { content: null, details: null, usage: null, structuredContent: null, isError: null, terminate: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")] },
  { name: "tool result extensions retained after hook", ...base, tools: [{ ...echo, result: result("original", { host: { receipt: 7 }, terminate: true }), after: { content: [text("replacement")], host: { receipt: 99 }, additional: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "partial result null content and extensions", ...base, tools: [{ ...echo, updates: [{ content: null, usage: null, progress: { fraction: 0.5 } }], result: result("final", { terminate: true }) }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")] },
  { name: "after content removes null structured content", ...base, tools: [{ ...echo, result: { content: null, structuredContent: null, usage: null, terminate: true }, after: { content: [] } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after empty update retains null structured content", ...base, tools: [{ ...echo, result: { content: null, structuredContent: null, usage: null, terminate: true }, after: {} }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after nonnull fields replace nulls", ...base, tools: [{ ...echo, result: { content: null, details: null, usage: null, structuredContent: null, isError: null, terminate: true }, after: { content: [], details: {}, usage, structuredContent: { answer: 2 }, isError: false } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "changed tool declaration drops obsolete null fields", ...base, tools: [echo], prompts: [{ role: "system", content: "policy", toolsAdded: null, toolsRemoved: null, host: { retained: true } }, user("go")] },
  { name: "removed tool declaration drops null additions", ...base, messages: [{ role: "system", content: "previous", toolsAdded: [echo] }], prompts: [{ role: "system", content: "policy", toolsAdded: null, toolsRemoved: null }, user("go")] },
  { name: "unchanged nullable tool declaration keeps source shape", ...base, prompts: [{ role: "system", content: "policy", toolsAdded: null, toolsRemoved: null }, user("go")] },
  { name: "replaced stale tool declaration removes null removal field", ...base, prompts: [{ role: "system", content: "policy", toolsAdded: [echo], toolsRemoved: null }, user("go")] },
  { name: "before hook executes changed argument without revalidation", ...base, tools: [{ ...echo, beforeArgs: { value: 123 } }], responses: [assistant([call("one", "echo", { value: "hello" })], "toolUse"), assistant()], recordHooks: true },
  { name: "before hook can change argument shape", ...base, tools: [{ ...echo, beforeArgs: { value: { nested: true } } }], responses: [assistant([call("one", "echo", { value: "hello" })], "toolUse"), assistant()], recordHooks: true },
  { name: "before hook can delete required argument", ...base, tools: [{ ...echo, beforeDeleteArgs: ["value"] }], responses: [assistant([call("one", "echo", { value: "hello" })], "toolUse"), assistant()], recordHooks: true },
  { name: "after hook mutates result without override", ...base, tools: [{ ...echo, result: result("original"), afterMutation: { content: [text("mutated")], details: { changed: true }, structuredContent: { answer: 3 }, usage, terminate: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after override merges onto mutated result", ...base, retainAfterResults: true, tools: [{ ...echo, result: result("original", { host: { receipt: 7 } }), afterMutation: { content: [text("mutated")], details: { changed: true }, structuredContent: { answer: 3 }, terminate: true }, after: { content: [text("overridden")] } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after result error flag mutation keeps computed outcome", ...base, tools: [{ ...echo, result: result("original", { terminate: true }), afterMutation: { isError: true } }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
  { name: "after failure replaces mutated result", ...base, retainAfterResults: true, tools: [{ ...echo, result: result("original"), afterMutation: { content: [text("mutated")], terminate: true }, afterFailure: "after failed" }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse"), assistant()], recordHooks: true },
  { name: "after hook deletes result fields", ...base, tools: [{ ...echo, result: result("original", { usage, terminate: true, structuredContent: { answer: 3 } }), afterDeleteResult: ["usage", "terminate", "structuredContent"] }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse"), assistant()], recordHooks: true },
  { name: "after context result replacement keeps executed object", ...base, retainAfterResults: true, tools: [{ ...echo, result: result("original"), afterMutation: { content: [text("mutated")], terminate: true }, afterContextResult: result("ignored") }], responses: [assistant([call("one", "echo", { value: "x" })], "toolUse")], recordHooks: true },
];

for (const mode of ["sequential", "parallel"]) {
  const tool = {...echo, result: result("original")};
  const turn = assistant([call("one", "echo", {value: "x"})], "toolUse");
  const common = {...base, mode, tools: [tool], responses: [turn, assistant()]};
  for (const [name, changes] of [
    ["content and metadata", {endMutation: {content: [text("redacted")], details: {redacted: true}, structuredContent: {answer: 9}, usage: {...usage, input: 7}}}],
    ["termination", {endMutation: {terminate: true}}],
    ["remove termination", {tools: [{...tool, result: result("original", {terminate: true})}], endDeleteResult: ["terminate"]}],
    ["error flags keep computed outcome", {endMutation: {isError: true}, endIsError: true}],
    ["event result replacement is local", {endReplacement: result("ignored", {terminate: true})}],
    ["immediate blocked result", {recordHooks: true, tools: [{...tool, before: {block: true, reason: "blocked", terminate: true}}], endMutation: {content: [text("redacted")], terminate: false}}],
    ["retained after result", {recordHooks: true, retainAfterResults: true, endMutation: {content: [text("redacted")], terminate: true}}],
    ["after override detaches result", {recordHooks: true, retainAfterResults: true, tools: [{...tool, after: {details: {overridden: true}}}], endMutation: {content: [text("redacted")], terminate: true}}],
    ["truncated result", {responses: [assistant([call("one", "echo", {value: "x"})], "length"), assistant()], endMutation: {content: [text("redacted")], terminate: true}}],
  ] as [string, any][]) cases.push({name: `tool end mutation/${mode}/${name}`, ...common, ...changes});
}
for (const [i, name] of ["quote\"", "slash\\", "line\n", "tab\t", "nul\0", "雪", "😀", "line\u2028separator"].entries()) {
  for (const kind of ["validation", "truncated", "unknown"]) cases.push({
    name: `tool name diagnostic/${i}/${kind}`, ...base,
    tools: kind === "unknown" ? [] : [{...echo, name}],
    responses: [assistant([call("one", name, {})], kind === "truncated" ? "length" : "toolUse"), assistant()],
  });
}

for (const mode of ["sequential", "parallel"]) {
  for (const [name, mutation, deleted] of [
    ["content and metadata", {content: [text("later callback")], details: {later: true}, usage: {...usage, input: 9}}, []],
    ["clear termination", {terminate: false}, []],
    ["delete termination and usage", {}, ["terminate", "usage"]],
    ["error flag keeps outcome", {isError: true}, []],
  ] as [string, any, string[]][]) cases.push({
    name: `retained tool result/${mode}/${name}`, ...base, mode,
    tools: [{...echo, result: result("original", {usage})}],
    responses: [assistant([call("one", "echo", {value: "a"}), call("two", "echo", {value: "b"})], "toolUse"), assistant()],
    waitForSecond: mode === "parallel", endMutation: {terminate: true},
    retainedEndAt: "tool_execution_end", retainedEndTrigger: mode === "parallel" ? "one" : "two",
    retainedEndTarget: mode === "parallel" ? "two" : "one", retainedEndMutation: mutation, retainedEndDelete: deleted,
  });
}
for (const event of ["message_start", "message_end"]) cases.push({
  name: `retained tool result/parallel/${event}`, ...base, mode: "parallel", waitForSecond: true,
  tools: [{...echo}], responses: [assistant([call("one", "echo", {value: "a"}), call("two", "echo", {value: "b"})], "toolUse"), assistant()],
  endMutation: {terminate: true}, retainedEndAt: event, retainedEndTrigger: "one", retainedEndTarget: "two",
  retainedEndMutation: {content: [text("changed before publication")], terminate: false},
});

for (const mode of ["sequential", "parallel"]) cases.push({
  name: `retained tool result/${mode}/immediate unknown tool`, ...base, mode,
  tools: [{...echo}], responses: [assistant([call("one", "missing"), call("two", "echo", {value: "b"})], "toolUse"), assistant()],
  endMutation: {terminate: true}, retainedEndAt: "tool_execution_end", retainedEndTrigger: "two", retainedEndTarget: "one",
  retainedEndMutation: {content: [text("revised missing tool")], terminate: false},
});

// Lifecycle callbacks may mutate a message object, while assistant start/update
// events deliberately expose a shallow top-level copy in the source.
for (const at of ["message_start", "message_end"]) {
  for (const source of ["prompt", "system", "steering", "prepared"]) cases.push({
    name: `message mutation/${source}/${at}`, ...base,
    ...(source === "system" ? {prompts: [{role: "system", content: "policy", timestamp: now}, user("go")]} : {}),
    ...(source === "steering" ? {prompts: [], steering: [[user("steered")]]} : {}),
    ...(source === "prepared" ? {prompts: [], responses: [assistant(), assistant()], decisions: ["continue", "end"], nextUpdates: [{messages: [user("prepared")]}]} : {}),
    messageMutationAt: at, messageMutationRole: source === "system" ? "system" : "user",
    messageMutation: {content: "callback revision", timestamp: now + 9},
  });
  for (const mode of ["sequential", "parallel"]) cases.push({
    name: `message mutation/tool result/${mode}/${at}`, ...base, mode, tools: [echo],
    responses: [assistant([call("one", "echo", {value: "x"})], "toolUse"), assistant()],
    messageMutationAt: at, messageMutationRole: "toolResult",
    messageMutation: {content: [text("callback revision")], timestamp: now + 9},
  });
}
for (const variant of ["terminal", "partial", "result-only"]) {
  for (const at of ["message_start", "message_update", "message_end"]) {
    if (at === "message_update" && variant !== "partial") continue;
    cases.push({
      name: `message mutation/assistant/${variant}/${at}`, ...base,
      responses: [assistant(), assistant()], decisions: ["continue", "end"],
      streamPartial: variant === "partial", omitTerminal: variant === "result-only",
      messageMutationAt: at, messageMutationRole: "assistant",
      messageMutation: {content: [text("callback revision")], timestamp: now + 9},
    });
  }
}
for (const stopReason of ["error", "aborted"]) cases.push({
  name: `message mutation/assistant/${stopReason}`, ...base,
  messageMutationAt: "message_end", messageMutationRole: "assistant",
  messageMutation: {stopReason, errorMessage: "callback failure"},
});

for (const [name, previous, current] of [
 ["distinct surrogate titles",String.raw`{"type":"object","title":"\ud800"}`,String.raw`{"type":"object","title":"\udc00"}`],
 ["distinct surrogate property names",String.raw`{"type":"object","properties":{"\ud800":{"type":"string"}}}`,String.raw`{"type":"object","properties":{"\udc00":{"type":"string"}}}`],
 ["same rounded number",'{"type":"object","x-scale":9007199254740992}','{"type":"object","x-scale":9007199254740993}'],
 ["same duplicate key value",'{"type":"object","title":"last"}','{"type":"object","title":"first","title":"last"}'],
]) cases.push({
 name: `tool declaration JSON/${name}`, ...base,
 messages: [{role:"system",content:"policy",timestamp:now,toolsAdded:[{...echo,parameters:JSON.parse(previous)}]}],
 tools: [{...echo,rawParameters:current}],
});

const clone = (value: any) => JSON.parse(JSON.stringify(value));
const output = [];
for (const input of cases) {
  const events: any[] = [], requests: any[] = [], hooks: any[] = [], executed: string[] = [], late: (() => void)[] = [];
  const afterResults: any[] = [];
  const endResults = new Map<string, any>();
  let responseIndex = 0, steeringIndex = 0, followIndex = 0, finishIndex = 0, nextIndex = 0, requestIndex = 0, keyIndex = 0;
  const controller = new AbortController();
  let secondDone!: () => void;
  const second = new Promise<void>(resolve => secondDone = resolve);
  const tools = input.tools.map((spec: any) => ({
    name: spec.name, description: spec.description, parameters: spec.rawParameters ? JSON.parse(spec.rawParameters) : spec.parameters, executionMode: spec.executionMode,
    ...(spec.prepare ? { prepareArguments: () => clone(spec.prepare) } : {}),
    execute: async (id: string, args: any, _signal: any, update: any) => {
      executed.push(id);
      for (const partial of spec.updates ?? []) update(clone(partial));
      if (spec.lateUpdate) late.push(() => update(clone(spec.lateUpdate)));
      if (input.waitForSecond && id === "one") await second;
      if (spec.failure) throw new Error(spec.failure);
      return clone(spec.result ?? result(JSON.stringify(args)));
    },
  }));
  const config: any = {
    model, reasoning: input.reasoning, apiKey: input.apiKey, toolExecution: input.mode,
    convertToLlm: (messages: any[]) => {
      hooks.push({ hook: "convert", roles: messages.map(m => m.role) });
      return messages.map(m => input.convertCustom && m.role === "custom" ? { ...m, role: "user" } : m);
    },
    getSteeringMessages: async () => { hooks.push({ hook: "steering" }); return clone(input.steering?.[steeringIndex++] ?? []); },
    getFollowUpMessages: async () => { hooks.push({ hook: "followUp" }); return clone(input.followUps?.[followIndex++] ?? []); },
    finishTurn: async (turn: any) => {
      hooks.push({ hook: "finish", stopReason: turn.message.stopReason, tools: turn.toolResults.map((m: any) => m.toolCallId) });
      const action = input.decisions?.[finishIndex++]; return action ? { action } : undefined;
    },
    prepareNextTurn: async () => { hooks.push({ hook: "next" }); return clone(input.nextUpdates?.[nextIndex++] ?? null) ?? undefined; },
    prepareRequest: async () => { hooks.push({ hook: "request" }); return clone(input.requestUpdates?.[requestIndex++] ?? null) ?? undefined; },
  };
  if (input.transformPrefix) config.transformContext = async (messages: any[]) => { hooks.push({ hook: "transform" }); return [clone(input.transformPrefix), ...messages]; };
  if (input.apiKeys) config.getApiKey = (provider: string) => { hooks.push({ hook: "key", provider }); return input.apiKeys[keyIndex++]; };
  if (input.recordHooks) {
    config.beforeToolCall = async (value: any) => {
      hooks.push({ hook: "before", id: value.toolCall.id, raw: clone(value.toolCall.arguments), args: clone(value.args) });
      if (input.cancelBefore === value.toolCall.id) controller.abort();
      const spec = input.tools.find((s: any) => s.name === value.toolCall.name);
      if (spec?.beforeArgs) Object.assign(value.args, clone(spec.beforeArgs));
      for (const key of spec?.beforeDeleteArgs ?? []) delete value.args[key];
      if (spec?.beforeFailure) throw new Error(spec.beforeFailure);
      return spec?.before;
    };
    config.afterToolCall = async (value: any) => {
      const spec = input.tools.find((s: any) => s.name === value.toolCall.name);
      hooks.push({ hook: "after", id: value.toolCall.id, isError: value.isError, ...(spec?.beforeArgs || spec?.beforeDeleteArgs ? { args: clone(value.args) } : {}) });
      if (input.retainAfterResults) afterResults.push(value.result);
      if (spec?.afterMutation) Object.assign(value.result, clone(spec.afterMutation));
      for (const key of spec?.afterDeleteResult ?? []) delete value.result[key];
      if (spec?.afterContextResult) value.result = clone(spec.afterContextResult);
      if (spec?.afterFailure) throw new Error(spec.afterFailure);
      return spec?.after;
    };
  }
  const stream = async (requestedModel: any, context: any, options: any) => {
    if (responseIndex >= input.responses.length) throw new Error("oracle script exhausted");
    const { signal: _signal, ...forwarded } = options;
    requests.push(clone({ model: requestedModel, context, options: forwarded }));
    const response = clone(input.responses[responseIndex++]);
    const events = new AssistantMessageEventStream();
    if (input.streamPartial) {
      const partial = { ...clone(response), content: [] };
      events.push({ type: "start", partial });
      events.push({ type: "text_delta", contentIndex: 0, delta: "done", partial: clone(response) });
    }
    if (input.omitTerminal) events.end(response);
    else if (["error", "aborted"].includes(response.stopReason)) events.push({ type: "error", reason: response.stopReason, error: response });
    else events.push({ type: "done", reason: response.stopReason, message: response });
    return events;
  };
  let messages: any[] | undefined, error: string | undefined;
  let messageMutated = false;
  const sink = async (event: any) => {
    events.push(clone(event));
    if (!messageMutated && event.type === input.messageMutationAt && event.message?.role === input.messageMutationRole) {
      Object.assign(event.message, clone(input.messageMutation));
      messageMutated = true;
    }
    if (event.type === input.failEvent) throw new Error("sink failed");
    if (event.type === "tool_execution_end") {
      if (input.endMutation) Object.assign(event.result, clone(input.endMutation));
      for (const key of input.endDeleteResult ?? []) delete event.result[key];
      if (input.endReplacement) event.result = clone(input.endReplacement);
      if (input.endIsError !== undefined) event.isError = input.endIsError;
      endResults.set(event.toolCallId, event.result);
    }
    if (event.type === input.retainedEndAt && (event.toolCallId ?? event.message?.toolCallId) === input.retainedEndTrigger) {
      const retained = endResults.get(input.retainedEndTarget);
      if (!retained) throw new Error("missing retained tool result");
      Object.assign(retained, clone(input.retainedEndMutation ?? {}));
      for (const key of input.retainedEndDelete ?? []) delete retained[key];
    }
    if (event.type === "tool_execution_end" && event.toolCallId === "two") secondDone();
  };
  try {
    const context = { messages: clone(input.messages), tools };
    messages = input.resume ? await runAgentLoopContinue(context, config, sink, controller.signal, input.omitStream ? undefined : stream) : await runAgentLoop(clone(input.prompts), context, config, sink, controller.signal, input.omitStream ? undefined : stream);
  } catch (failure) { error = (failure as Error).message; }
  for (const update of late) update();
  output.push({ input, expected: { events, requests, hooks, executed: executed.sort(), ...(messages ? { messages } : {}), ...(error ? { error } : {}), ...(input.retainAfterResults ? { afterResults } : {}) } });
}
const destination = new URL("pi-loop.json", import.meta.url);
// One case per line makes regeneration diffs bounded while preserving every
// event, provider request, hook, and result without lossy projections.
const serialized = `{\n  "upstreamCommit": ${JSON.stringify(manifest.commit)},\n  "cases": [\n${output.map(value => "    " + JSON.stringify(value)).join(",\n")}\n  ]\n}\n`;
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Pi loop fixtures changed; regenerate and review");
} else writeFileSync(destination, serialized);
console.log(`Verified ${output.length} native-loop fixtures against Pi ${manifest.commit}`);
