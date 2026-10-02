import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createServer } from "node:http";
import { once } from "node:events";
import * as core from "../dist/agentcore.mjs";
import { InMemoryTelemetryContext } from "../dist/telemetry.mjs";
import { createTelemetryAdapterConformance } from "../dist/telemetry-testing.mjs";
import { PiWorker } from "../dist/bridge.mjs";
import { nativeStream } from "../dist/stream.mjs";

const copy = (value) => JSON.parse(JSON.stringify(value));
const assistant = (content, stopReason = "stop") => ({
  role: "assistant", content, stopReason, timestamp: 100,
  api: "test", provider: "test", model: "test",
  usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
});
const text = (text) => assistant([{ type: "text", text }]);
const tool = { name: "echo", label: "Echo", description: "Echo input", parameters: {
  type: "object", properties: { value: { type: "string" } }, required: ["value"], additionalProperties: false,
} };
const toolResult = (args) => ({ content: [{ type: "text", text: args.value }], details: { echoed: args.value } });

function stream(message) {
  return {
    async *[Symbol.asyncIterator]() {
      yield { type: "start", partial: message };
      yield { type: "done", reason: message.stopReason, message };
    },
    result: async () => message,
  };
}

function client(callback) {
  let id = 0;
  const pending = new Map();
  const worker = new PiWorker((message) => {
    if (message.kind === "callback") {
      void Promise.resolve().then(() => callback(message.method, copy(message.params))).then(
        (value) => worker.receive({ kind: "result", id: message.id, value }),
        (error) => worker.receive({ kind: "result", id: message.id, error: { name: error.name, message: error.message } }),
      );
    } else if (message.kind === "result") {
      const waiter = pending.get(message.id);
      pending.delete(message.id);
      if (message.error) waiter.reject(new Error(message.error.message));
      else waiter.resolve(message.value === undefined ? undefined : copy(message.value));
    }
  });
  return {
    worker,
    call(method, params) {
      return new Promise((resolve, reject) => {
        const key = `test-${++id}`;
        pending.set(key, { resolve, reject });
        worker.receive({ kind: "call", id: key, method, params });
      });
    },
  };
}

test("built native entry exposes the entire executable agent surface", async () => {
  for (const name of ["Agent", "agentLoop", "agentLoopContinue", "runAgentLoop", "runAgentLoopContinue", "runToolCall", "setDefaultStreamFn", "streamProxy"]) {
    assert.equal(typeof core[name], "function", name);
  }
  const build = JSON.parse(await readFile(new URL("../dist/build.json", import.meta.url), "utf8"));
  const pin = JSON.parse(await readFile(new URL("../UPSTREAM.json", import.meta.url), "utf8"));
  assert.equal(build.commit, pin.commit);
  assert.ok(Object.keys(build.inputs).some((file) => file.endsWith("upstream/packages/agent/src/agent-loop.ts")));
  assert.ok(Object.keys(build.inputs).some((file) => file.endsWith("upstream/packages/telemetry/src/memory.ts")));
  assert.ok(Object.keys(build.inputs).every((file) => !file.includes(".work/") && !file.includes("node_modules/@earendil-works/")));
});

test("bridge and native Agent produce identical serialized events and state", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: 100 });
  const replies = [assistant([{ type: "toolCall", id: "echo-1", name: "echo", arguments: { value: "from tool" } }], "toolUse"), text("complete")];
  const nativeEvents = [];
  const nativeReplies = copy(replies);
  const native = new core.Agent({
    initialState: { systemPrompt: "Test agent", tools: [{ ...tool, execute: async (_id, args) => toolResult(args) }] },
    streamFn: () => stream(nativeReplies.shift()),
  });
  native.subscribe((event) => { nativeEvents.push(copy(event)); });
  await native.prompt("test");

  const bridgeEvents = [];
  const traces = [];
  const bridgeReplies = copy(replies);
  const bridge = client((method, params) => {
    switch (method) {
      case "stream": return bridgeReplies.shift();
      case "tool": return toolResult(params.args);
      case "event": bridgeEvents.push(params); return;
      case "trace": traces.push(params); throw new Error("observer failure");
      default: throw new Error(`unexpected callback ${method}`);
    }
  });
  t.after(() => bridge.worker.close());
  await bridge.call("initialize", { traceRequests: true, initialState: { systemPrompt: "Test agent", tools: [tool] } });
  await bridge.call("prompt", { input: "test" });
  assert.deepEqual(bridgeEvents, nativeEvents);
  assert.equal(traces.length, 2);
  assert.deepEqual(traces.map((trace) => trace.response), replies);
  assert.ok(traces.every((trace) => trace.spans.length && trace.spans.every((span) => span.settled)));
  assert.deepEqual(await bridge.call("state"), copy({ ...native.state, pendingToolCalls: [...native.state.pendingToolCalls] }));
  const spans = await bridge.call("telemetry");
  assert.deepEqual(spans.map((span) => span.name), ["agentray.agent.run", "agentray.ai.request", "agentray.tool.execute", "agentray.ai.request"]);
  assert.ok(spans.every((span) => span.settled));
  assert.ok(spans.slice(1).every((span) => span.parentId === spans[0].id));
});

test("prepared replacement tools execute through the host callback", async (t) => {
  let called = 0;
  const bridge = client((method, params) => {
    switch (method) {
      case "prepareRequest": return { context: { ...params.context, tools: [tool] } };
      case "stream": return assistant([{ type: "toolCall", id: "echo-1", name: "echo", arguments: { value: "replacement" } }], "toolUse");
      case "tool": called++; return { ...toolResult(params.args), terminate: true };
      case "event": return;
      default: throw new Error(`unexpected callback ${method}`);
    }
  });
  t.after(() => bridge.worker.close());
  await bridge.call("initialize", { callbacks: ["prepareRequest"] });
  await bridge.call("prompt", { input: "test" });
  assert.equal(called, 1);
});

test("provider callback rejection preserves the native failure lifecycle", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: 100 });
  const nativeEvents = [];
  const native = new core.Agent({ streamFn: async () => { throw new Error("provider failed"); } });
  native.subscribe((event) => { nativeEvents.push(copy(event)); });
  await native.prompt("test");
  const bridgeEvents = [];
  const bridge = client((method, params) => {
    if (method === "event") { bridgeEvents.push(params); return; }
    throw new Error("provider failed");
  });
  t.after(() => bridge.worker.close());
  await bridge.call("initialize", {});
  await bridge.call("prompt", { input: "test" });
  assert.deepEqual(bridgeEvents, nativeEvents);
  assert.deepEqual(await bridge.call("state"), copy({ ...native.state, pendingToolCalls: [...native.state.pendingToolCalls] }));
  assert.ok((await bridge.call("telemetry")).every((span) => span.settled && span.status.status === "error"));
});

test("native provider mode retains original HTTP requests, events, state, and settled telemetry", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: 100 });
  const requests = [];
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    requests.push({ path: request.url, authorization: request.headers.authorization,
      body: JSON.parse(Buffer.concat(chunks).toString()) });
    response.writeHead(200, { "content-type": "text/event-stream" });
    response.write(`data: ${JSON.stringify({ id: "native", choices: [{ index: 0, delta: { role: "assistant", content: "native answer" }, finish_reason: null }] })}\n\n`);
    response.write(`data: ${JSON.stringify({ id: "native", choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } })}\n\n`);
    response.end("data: [DONE]\n\n");
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => { server.closeAllConnections(); server.close(); });
  const initialState = { systemPrompt: "native provider test", model: {
    id: "test", name: "test", api: "openai-completions", provider: "test",
    baseUrl: `http://127.0.0.1:${server.address().port}/v1`, reasoning: false,
    input: ["text"], contextWindow: 8192, maxTokens: 512,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  } };
  const nativeEvents = [];
  const streamOptions = { maxTokens: 123, temperature: 0.25 };
  const payloads = [];
  const onPayload = (payload, model) => {
    payloads.push(copy({ payload, model }));
    return { ...payload, response_format: { type: "json_object" }, metadata: { nativeHook: true } };
  };
  const native = new core.Agent({ initialState: copy(initialState),
    streamFn: (model, context, options) => nativeStream(model, context, { ...streamOptions, ...options }),
    getApiKey: () => "local-test-key", onPayload });
  native.subscribe((event) => { nativeEvents.push(copy(event)); });
  await native.prompt("test");
  const bridgeEvents = [];
  const bridge = client((method, params) => {
    if (method === "getApiKey") return "local-test-key";
    if (method === "onPayload") return onPayload(params.payload, params.model);
    if (method === "event") { bridgeEvents.push(params); return; }
    throw new Error(`native mode invoked ${method}`);
  });
  t.after(() => bridge.worker.close());
  await bridge.call("initialize", { initialState: copy(initialState), streamMode: "native", streamOptions, callbacks: ["getApiKey", "onPayload"] });
  await bridge.call("prompt", { input: "test" });
  assert.equal(requests.length, 2);
  assert.deepEqual(requests[0], requests[1]);
  assert.equal(requests[1].path, "/v1/chat/completions");
  assert.equal(requests[1].authorization, "Bearer local-test-key");
  assert.equal(requests[1].body.max_tokens ?? requests[1].body.max_completion_tokens, 123);
  assert.equal(requests[1].body.temperature, 0.25);
  assert.deepEqual(payloads[0], payloads[1]);
  assert.deepEqual(payloads[1].model, initialState.model);
  assert.deepEqual(requests[1].body.response_format, { type: "json_object" });
  assert.deepEqual(requests[1].body.metadata, { nativeHook: true });
  assert.deepEqual(bridgeEvents, nativeEvents);
  assert.deepEqual(await bridge.call("state"), copy({ ...native.state, pendingToolCalls: [...native.state.pendingToolCalls] }));
  const spans = await bridge.call("telemetry");
  assert.deepEqual(spans.map((span) => span.name), ["agentray.agent.run", "agentray.ai.request"]);
  assert.ok(spans.every((span) => span.settled));
  assert.ok(!JSON.stringify(spans).includes("local-test-key"));
});

test("native provider mode rejects unknown APIs without invoking a host provider", async (t) => {
  const bridge = client((method) => {
    if (method === "event") return;
    throw new Error(`unexpected host callback: ${method}`);
  });
  t.after(() => bridge.worker.close());
  await assert.rejects(bridge.call("initialize", { streamMode: "typo" }), /Unknown stream mode/);
  for (const reserved of ["apiKey", "signal", "telemetryContext", "fetch", "onResponse"]) {
    await assert.rejects(bridge.call("initialize", { streamOptions: { [reserved]: "invalid" } }), /belongs to the native runtime or its callback/);
  }
  await bridge.call("initialize", { streamMode: "native", initialState: { model: {
    api: "__proto__", id: "unsupported", provider: "test", cost: {},
  } } });
  await bridge.call("prompt", { input: "test" });
  const state = await bridge.call("state");
  assert.equal(state.isStreaming, false);
  assert.match(state.errorMessage, /Unsupported native Pi API: __proto__/);
  assert.equal(state.messages.at(-1).stopReason, "error");
  assert.ok((await bridge.call("telemetry")).every((span) => span.settled && span.status.status === "error"));
});

test("unknown synchronous tool preparers fail during initialization", async (t) => {
  const bridge = client(() => { throw new Error("initialization must not call a host"); });
  t.after(() => bridge.worker.close());
  await assert.rejects(bridge.call("initialize", { initialState: {
    tools: [{ ...tool, agentrayPrepareArguments: "unregistered-module" }],
  } }), /Unknown native argument preparer/);
});

for (const c of createTelemetryAdapterConformance(async () => {
  const context = new InMemoryTelemetryContext();
  return { context, getSpans: async () => context.getSpans(), async [Symbol.asyncDispose]() {} };
})) {
  test(`built telemetry: ${c.group}: ${c.name}`, () => c.run());
}
