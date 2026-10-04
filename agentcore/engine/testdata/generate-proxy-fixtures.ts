import { readFileSync, writeFileSync } from "node:fs";
import { root, manifest } from "./oracle.ts";
const { streamProxy } = await import(new URL("upstream/packages/agent/src/proxy.ts", root).pathname);
const now = 1700000000123;
Date.now = () => now;
const model = { id: "proxy-model", api: "openai-responses", provider: "test", custom: { retained: true } };
const context = { messages: [] };
const usage = { input: 5, output: 7, cacheRead: 2, cacheWrite: 0, totalTokens: 14, cost: { input: 0.001, output: 0.002, cacheRead: 0, cacheWrite: 0, total: 0.003 } };
const done = { type: "done", reason: "stop", usage };
const start = { type: "start" };
const textStart = { type: "text_start", contentIndex: 0 };
const toolStart = { type: "toolcall_start", contentIndex: 0, id: "call_1", toolName: "lookup" };
const frame = (type: string, rest = {}) => ({ type, contentIndex: 0, ...rest });
const options = { temperature: 0, maxTokens: 12, reasoning: "high", cacheRetention: "long", sessionId: "session", headers: { "x-test": "yes" }, metadata: { task: 1 }, transport: "sse", thinkingBudgets: { high: 200 }, maxRetryDelayMs: 0, samplingParams: { topP: 0.9 }, apiKey: "must-not-forward", signal: undefined, unsupported: true };
const inputs: any[] = [
 {name: "text-signature", frames: [start, textStart, frame("text_delta", {delta: "Chào 🌏"}), frame("text_end", {contentSignature: "sig"}), done]},
 {name: "thinking", frames: [start, frame("thinking_start"), frame("thinking_delta", {delta: "think"}), frame("thinking_end", {contentSignature: ""}), {...done, providerThinkingLevel: "high"}]},
 {name: "tool-metadata", frames: [start, toolStart, frame("toolcall_delta", {delta: '{"value":"hello"}'}), frame("toolcall_end", {toolCall: {namespace: "dynamic_tools", thoughtSignature: "sig", extension: {n: 1}}}), {...done, reason: "toolUse"}]},
 {name: "partial-tool-json", frames: [start, toolStart, frame("toolcall_delta", {delta: '{"value": "hel'}), {...done, reason: "length"}]},
 {name: "falsy-tool-json", frames: [start, toolStart, frame("toolcall_delta", {delta: "false"}), done]},
 {name: "empty-signature-removes", frames: [start, textStart, frame("text_end", {contentSignature: "old"}), frame("text_end"), done]},
 {name: "terminal-without-newline", frames: [start, {...done, providerThinkingLevel: "low"}], noNewline: true},
 {name: "early-eof", frames: [start, textStart, frame("text_delta", {delta: "unfinished"})]},
 {name: "empty-body", frames: []},
 {name: "provider-error", frames: [start, {...done, type: "error", reason: "error", errorMessage: "provider failed", providerThinkingLevel: "low"}]},
 {name: "provider-aborted", frames: [start, {...done, type: "error", reason: "aborted"}]},
 {name: "wrong-text-delta", frames: [start, frame("text_delta", {delta: "bad"})]},
 {name: "wrong-text-end", frames: [start, toolStart, frame("text_end")]},
 {name: "wrong-thinking-delta", frames: [start, textStart, frame("thinking_delta", {delta: "bad"})]},
 {name: "wrong-thinking-end", frames: [start, frame("thinking_end")]},
 {name: "wrong-tool-delta", frames: [start, textStart, frame("toolcall_delta", {delta: "bad"})]},
 {name: "wrong-tool-end-ignored", frames: [start, frame("toolcall_end", {toolCall: {name: "bad"}}), done]},
 {name: "sparse-blocks", frames: [start, frame("text_start", {contentIndex: 2}), frame("text_delta", {contentIndex: 2, delta: "third"}), done]},
 {name: "duplicate-start-replaces-block", frames: [start, textStart, frame("text_delta", {delta: "old"}), textStart, frame("text_delta", {delta: "new"}), done]},
 {name: "post-terminal-update", frames: [start, textStart, done, frame("text_delta", {delta: "after"}), {...done, providerThinkingLevel: "later"}]},
 {name: "post-terminal-error", frames: [start, done, frame("text_delta", {delta: "bad"})]},
 {name: "tool-pointer-after-append", frames: [start, toolStart, frame("toolcall_end", {toolCall: {}}), frame("text_start", {contentIndex: 1}), frame("toolcall_end", {toolCall: {namespace: "later"}}), done]},
 {name: "tool-pointer-after-replace", frames: [start, toolStart, frame("toolcall_end", {toolCall: {}}), frame("toolcall_delta", {delta: "{}"}), frame("toolcall_end", {toolCall: {namespace: "new-object"}}), done]},
 {name: "options-allowlist", frames: [done], options},
 {name: "http-json-error", status: 429, statusText: "Too Many Requests", body: '{"error":"quota"}'},
 {name: "http-text-error", status: 502, statusText: "Bad Gateway", body: "unavailable"},
 {name: "http-falsy-error", status: 400, statusText: "Bad Request", body: '{"error":false}'},
 {name: "http-object-error", status: 400, statusText: "Bad Request", body: '{"error":{"detail":"bad"}}'},
 {name: "http-fixed-number-error", status: 400, statusText: "Bad Request", body: '{"error":1000000}'},
 {name: "http-small-number-error", status: 400, statusText: "Bad Request", body: '{"error":1e-7}'},
 {name: "http-nested-array-error", status: 400, statusText: "Bad Request", body: '{"error":["failed",null,[1000000,true],{"code":7}]}'},
 {name: "http-invalid-trailing-json", status: 400, statusText: "Bad Request", body: '{"error":"ignore me"} trailing'},
 {name: "javascript-trim", body: 'data: \uFEFF'+JSON.stringify(done)+'\uFEFF\n'},
 {name: "strict-data-prefix", body: ': keepalive\ndata:{"type":"done"}\n data: {}\ndata:   \n'},
 {name: "bom-and-crlf", body: '\uFEFFdata: '+JSON.stringify(start)+'\r\n\r\ndata: '+JSON.stringify(done)+'\r\n'},
 {name: "terminal-usage-extensions", frames: [start, {...done, usage: {...usage, providerMeter: {requests: 1}, cost: {...usage.cost, currency: "USD"}}}]},
 {name: "terminal-sparse-usage", frames: [start, {...done, usage: {input: 5, cost: {input: 0.1}}}]},
 {name: "terminal-empty-usage", frames: [start, {...done, usage: {}}]},
 {name: "terminal-null-usage", frames: [start, {...done, usage: null}]},
 {name: "terminal-omitted-usage", frames: [start, {type: "done", reason: "stop"}]},
 {name: "terminal-null-thinking", frames: [start, {...done, providerThinkingLevel: null}]},
 {name: "terminal-thinking-removal", frames: [start, {...done, providerThinkingLevel: "high"}, {...done, providerThinkingLevel: null}]},
 {name: "terminal-thinking-retained", frames: [start, {...done, providerThinkingLevel: "high"}, done]},
 {name: "error-null-message", frames: [start, {...done, type: "error", reason: "error", errorMessage: null}]},
 {name: "error-message-removal", frames: [start, {...done, type: "error", reason: "error", errorMessage: "old"}, {...done, type: "error", reason: "error"}]},
 {name: "text-null-signature", frames: [start, textStart, frame("text_end", {contentSignature: null}), done]},
 {name: "text-null-signature-removal", frames: [start, textStart, frame("text_end", {contentSignature: null}), frame("text_end"), done]},
 {name: "text-null-signature-replacement", frames: [start, textStart, frame("text_end", {contentSignature: null}), frame("text_end", {contentSignature: "new"}), done]},
 {name: "thinking-null-signature", frames: [start, frame("thinking_start"), frame("thinking_end", {contentSignature: null}), done]},
 {name: "thinking-null-signature-removal", frames: [start, frame("thinking_start"), frame("thinking_end", {contentSignature: null}), frame("thinking_end"), done]},
 {name: "thinking-null-signature-replacement", frames: [start, frame("thinking_start"), frame("thinking_end", {contentSignature: null}), frame("thinking_end", {contentSignature: "new"}), done]},
];
for (const failureAt of ["fetch", "read"]) {
 for (const failure of [
  {kind: "error", message: "transport failed"},
  {kind: "value", value: null},
  {kind: "value", value: false},
  {kind: "value", value: 0},
  {kind: "value", value: ""},
  {kind: "value", value: {message: "not an Error"}},
  {kind: "value", value: ["failed", null, {code: 7}]},
 ]) inputs.push({name: `${failureAt}-throws-${JSON.stringify(failure)}`, frames: [], failureAt, failure});
}
for (const failure of [
 {kind: "error", message: "request serialization failed"},
 {kind: "value", value: null},
 {kind: "value", value: false},
 {kind: "value", value: 0},
 {kind: "value", value: ""},
 {kind: "value", value: "request failed"},
 {kind: "value", value: {message: "not an Error"}},
 {kind: "value", value: ["failed", null, {code: 7}]},
]) inputs.push({name: `request-serialization-throws-${JSON.stringify(failure)}`, frames: [], requestFailure: failure});
inputs.push({name: "request-serialization-returned-error", frames: [], requestFailure: {kind: "error", message: "request serialization failed"}, requestFailureMode: "return"});
for (const failure of [{kind: "error", message: "cancelled serialization"}, {kind: "value", value: null}]) inputs.push({
 name: `request-serialization-aborted-${failure.kind}`, frames: [], requestFailure: failure, abortOnRequest: true,
});
inputs.push({name: "request-serialization-nested-returned-error", frames: [], requestFailure: {kind: "error", message: "request serialization failed"}, requestFailureMode: "nested-return"});
inputs.push({name: "request-serialization-clock-order", frames: [done], requestClockAdvance: true});
inputs.push({name: "request-serialization-model-order", frames: [done], requestModelMutation: true});
inputs.push({name: "request-clock-model-order", frames: [done], clockModelMutation: true});
for (const number of [
 {value: "NaN"}, {value: "Infinity"}, {value: "-Infinity"}, {value: "-0"},
 {value: "0.000001"}, {value: "1e-7"}, {value: "1e21"}, {value: "5e-324"},
 {value: "0.1", kind: "float32"}, {value: "9007199254740993", kind: "int64"},
 {value: "9223372036854775807", kind: "int64"}, {value: "18446744073709551615", kind: "uint64"},
]) for (const nested of [false, true]) inputs.push({
 name: `request-number/${number.kind ?? "float64"}/${number.value}/${nested ? "nested" : "scalar"}`,
 frames: [done], requestNumber: {...number, nested}, rawRequest: true,
});
inputs.push({name: "request-option-serializer-order", frames: [done], requestOptionOrder: true, rawRequest: true});
inputs.push({name: "request-unescaped-string", frames: [done], options: {metadata: {value: "<tag>&\u2028line\u2029end"}}, rawRequest: true});
for (const mutation of ["replace", "delete", "insert"]) inputs.push({name: `request-map-mutation/${mutation}`, frames: [done], requestMapMutation: mutation, rawRequest: true});
inputs.push({name: "request-shared-child", frames: [done], requestSharedValue: true, rawRequest: true});
for (const kind of ["map", "array", "mutating-map"]) inputs.push({name: `request-cycle/${kind}`, frames: [], requestCycle: kind});
for (const [name, raw] of [
 ["number overflow", '{"positive":1e309,"negative":-1e309}'],
 ["number underflow", '{"value":-1e-999}'],
 ["number spelling", '[1.0,1e0,1.25e2,-0,0e100]'],
 ["large integers", '[9007199254740993,9223372036854775807,18446744073709551615]'],
 ["duplicate keys", '{"z":1,"a":2,"z":3}'],
 ["escaped duplicate keys", String.raw`{"a":1,"\u0061":2}`],
 ["index key ordering", '{"z":0,"10":10,"2":2,"4294967295":3,"00":4,"0":5,"4294967294":6}'],
 ["nested key ordering", '{"outer":{"b":1,"1":2,"a":3,"0":4}}'],
 ["empty containers", '{"a":{},"b":[],"c":null}'],
 ["HTML and separators", String.raw`"\u003c\u003e\u0026\u2028\u2029"`],
 ["control escapes", String.raw`"\u0000\u0008\u0009\u000a\u000b\u000c\u000d\u001f"`],
 ["slash quote backslash", String.raw`"\/\u0022\u005c"`],
 ["surrogate pair", String.raw`"\uD83D\uDE00"`],
 ["lone high surrogate", String.raw`"\uD800"`],
 ["lone low surrogate", String.raw`"\uDC00"`],
 ["separated surrogates", String.raw`"\ud800x\udc00"`],
 ["multiple surrogates", String.raw`"\ud800\ud800\udc00\udc00"`],
 ["reversed surrogates", String.raw`"\udc00\ud800"`],
 ["surrogate object keys", String.raw`{"\ud800":1,"\udc00":2,"\ud800":3}`],
 ["astral duplicate keys", '{' + JSON.stringify("😀") + String.raw`:1,"\ud83d\ude00":2}`],
 ["Unicode literal and escape", String.raw`{"雪":"\u96ea","é":"\u00e9"}`],
 ["raw callback representation", String.raw`{"z":1e309,"a":"\uD800","2":-0,"1":9007199254740993}`],
] as [string, string][]) inputs.push({name: `request-raw-json/${name}`, frames: [done], rawMetadata: raw, rawMetadataCallback: name === "raw callback representation", rawRequest: true});
const results = [];
inputs.push(
 {name: "aborted-fetch-retains-failure", frames: [], failureAt: "fetch", abortOnFailure: true, failure: {kind: "error", message: "transport failed"}},
 {name: "aborted-reader-retains-failure", frames: [], failureAt: "read", abortOnFailure: true, failure: {kind: "value", value: {message: "not an Error"}}},
);
for (const input of inputs) {
 let clock = now;
 const requestedModel = {...model};
 Date.now = () => {if (input.clockModelMutation) requestedModel.id = "other-model"; return clock;};
 let request: any;
 const serializerOrder: string[] = [];
 const controller = new AbortController();
 const body = input.body ?? input.frames.map((frame: any) => `data: ${JSON.stringify(frame)}\n\n`).join("");
 input.body = input.noNewline ? body.trimEnd() : body;
 delete input.frames; delete input.noNewline;
 globalThis.fetch = (async (url: any, init: any) => {
  request = {url, method: init.method, headers: init.headers, body: JSON.parse(init.body), ...(input.rawRequest ? {rawBody: init.body} : {})};
  if (input.failure) {
   if (input.abortOnFailure) controller.abort();
   const failure = input.failure.kind === "error" ? new Error(input.failure.message) : input.failure.value;
   if (input.failureAt === "fetch") throw failure;
   return new Response(new ReadableStream({start(controller) {controller.error(failure);}}));
  }
  return new Response(input.body, {status: input.status ?? 200, statusText: input.statusText ?? "OK"});
 }) as any;
 const requestOptions = {...input.options};
 if (input.requestFailure) requestOptions.metadata = {toJSON() {
  if (input.abortOnRequest) controller.abort();
  const spec = input.requestFailure;
  throw spec.kind === "error" ? new Error(spec.message) : spec.value;
 }};
 if (input.requestClockAdvance) requestOptions.metadata = {toJSON() {clock += 1000; return {serialized: true};}};
 if (input.requestModelMutation) requestOptions.metadata = {toJSON() {requestedModel.id = "other-model"; return {serialized: true};}};
 if (input.requestNumber) {
  const spec = input.requestNumber;
  const value = spec.kind === "float32" ? Math.fround(Number(spec.value)) : Number(spec.value);
  if (spec.nested) requestOptions.metadata = {nested: [value]};
  else requestOptions.temperature = value;
 }
 if (input.requestOptionOrder) {
  for (const key of ["temperature", "samplingParams", "maxTokens", "reasoning", "cacheRetention", "sessionId", "headers", "metadata", "transport", "thinkingBudgets", "maxRetryDelayMs", "ignored"]) {
   requestOptions[key] = {toJSON() {serializerOrder.push(key); return key;}};
  }
 }
 if (input.requestMapMutation) {
  const metadata: any = {a: {toJSON() {
   if (input.requestMapMutation === "replace") metadata.b = 2;
   if (input.requestMapMutation === "delete") delete metadata.b;
   if (input.requestMapMutation === "insert") metadata.c = 3;
   return {serialized: true};
  }}, b: 1};
  requestOptions.metadata = metadata;
 }
 if (input.requestSharedValue) { const child = {value: 1}; requestOptions.metadata = {a: child, b: child}; }
 if (input.requestCycle) {
  const metadata: any = input.requestCycle === "array" ? [] : {};
  if (Array.isArray(metadata)) metadata.push(metadata);
  else {
   if (input.requestCycle === "mutating-map") metadata.a = {toJSON() {serializerOrder.push("a"); delete metadata.unused; return true;}};
   metadata.self = metadata;
   if (input.requestCycle === "mutating-map") metadata.unused = true;
  }
  requestOptions.metadata = metadata;
 }
 if (input.rawMetadata !== undefined) {
  requestOptions.metadata = input.rawMetadataCallback ? {toJSON() {return JSON.parse(input.rawMetadata);}} : JSON.parse(input.rawMetadata);
 }
 const stream = streamProxy(requestedModel, context as any, { ...requestOptions, signal: controller.signal, authToken: "test-token", proxyUrl: "https://proxy.example.com" });
 const events = [];
 for await (const event of stream) events.push(event);
 const result = await stream.result();
 // All input is one response chunk. Allow finally/end to run before capturing
 // retained live pointers, including mutations after the first terminal frame.
 await new Promise(resolve => setTimeout(resolve, 0));
 results.push({input, expected: JSON.parse(JSON.stringify({events, result, request, ...((input.requestOptionOrder || input.requestCycle === "mutating-map") ? {serializerOrder} : {})}))});
}
const destination = new URL("pi-proxy.json", import.meta.url);
const serialized = JSON.stringify({upstreamCommit: manifest.commit, model, now, cases: results}, null, 2)+"\n";
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== serialized) throw new Error("Pi proxy fixtures changed; regenerate and review");
} else writeFileSync(destination, serialized);
console.log(`Verified ${results.length} proxy fixtures against Pi ${manifest.commit}`);
