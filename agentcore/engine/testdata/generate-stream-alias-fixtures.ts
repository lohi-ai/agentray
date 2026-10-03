import { readFileSync, writeFileSync } from "node:fs";
import { root, aiRoot, manifest } from "./oracle.ts";
const { runAgentLoop } = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const { Agent } = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const { AssistantMessageEventStream } = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
const clone = (value: any) => JSON.parse(JSON.stringify(value));
Date.now = () => 1000;
const cases = [];
for (const mode of ["loop", "agent"]) for (const shape of ["single", "repeated", "distinct", "detached"]) {
  const call = { type: "toolCall", id: "call", name: "echo", arguments: {} };
  const content = shape === "single" || shape === "detached" ? [call] : [call, shape === "repeated" ? call : clone(call)];
  const model = { id: "test", api: "test", provider: "test" };
  const assistant = { role: "assistant", content, timestamp: 1, api: "test", provider: "test", model: "test", stopReason: "error", usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } };
  const stream = async () => {
    const stream = new AssistantMessageEventStream();
    stream.push({ type: "start", partial: assistant });
    stream.push({ type: "toolcall_end", contentIndex: 0, toolCall: shape === "detached" ? clone(call) : call, partial: assistant });
    stream.push({ type: "error", reason: "error", error: assistant });
    return stream;
  };
  const events: any[] = [], aliases: any[] = [];
  let messages: any[] = [];
  const sink = async (event: any) => {
    events.push(clone(event));
    if (event.message?.role === "assistant") {
      const blocks = event.message.content;
      aliases.push({ type: event.type, blocks: blocks.map((b: any) => blocks.indexOf(b)), ...(event.assistantMessageEvent ? { samePartial: event.message === event.assistantMessageEvent.partial, toolCallMatches: blocks.map((b: any) => b === event.assistantMessageEvent.toolCall) } : {}) });
    }
    if (event.type === "agent_end") messages = event.messages;
  };
  const prompt = { role: "user", content: "go", timestamp: 0 };
  const config = { model, convertToLlm: (messages: any[]) => messages };
  if (mode === "loop") messages = await runAgentLoop([prompt], { messages: [], tools: [] }, config, sink, undefined, stream);
  else {
    const agent = new Agent({ ...config, initialState: { model }, streamFn: stream });
    agent.subscribe(sink);
    await agent.prompt(prompt);
  }
  cases.push({ input: { mode, shape }, expected: { events, aliases, messages } });
}
const serialized = JSON.stringify({ upstreamCommit: manifest.commit, cases }, null, 2) + "\n";
const destination = new URL("pi-stream-aliases.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Stream alias fixtures changed");
} else writeFileSync(destination, serialized);
console.log(`Verified ${cases.length} stream-alias cases against Pi ${manifest.commit}`);
