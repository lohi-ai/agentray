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
const results = [];
for (const input of inputs) {
 let request: any;
 const body = input.body ?? input.frames.map((frame: any) => `data: ${JSON.stringify(frame)}\n\n`).join("");
 input.body = input.noNewline ? body.trimEnd() : body;
 delete input.frames; delete input.noNewline;
 globalThis.fetch = (async (url: any, init: any) => {
  request = {url, method: init.method, headers: init.headers, body: JSON.parse(init.body)};
  return new Response(input.body, {status: input.status ?? 200, statusText: input.statusText ?? "OK"});
 }) as any;
 const stream = streamProxy(model, context as any, { ...input.options, authToken: "test-token", proxyUrl: "https://proxy.example.com" });
 const events = [];
 for await (const event of stream) events.push(event);
 const result = await stream.result();
 // All input is one response chunk. Allow finally/end to run before capturing
 // retained live pointers, including mutations after the first terminal frame.
 await new Promise(resolve => setTimeout(resolve, 0));
 results.push({input, expected: JSON.parse(JSON.stringify({events, result, request}))});
}
const destination = new URL("pi-proxy.json", import.meta.url);
const serialized = JSON.stringify({upstreamCommit: manifest.commit, model, now, cases: results}, null, 2)+"\n";
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== serialized) throw new Error("Pi proxy fixtures changed; regenerate and review");
} else writeFileSync(destination, serialized);
console.log(`Verified ${results.length} proxy fixtures against Pi ${manifest.commit}`);
