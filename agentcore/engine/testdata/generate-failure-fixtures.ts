// Development oracle: all failure handling executes the unmodified Pi source.
import { readFileSync, writeFileSync } from "node:fs";
import { root, manifest } from "./oracle.ts";
const { Agent } = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const { runToolCall } = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
Date.now = () => 1700000000123;
const model = { id: "model", api: "api", provider: "provider" };
const values = [
  { name: "error", kind: "error", value: "failure" },
  { name: "null", value: null }, { name: "false", value: false },
  { name: "empty-string", value: "" }, { name: "string", value: "xin chào" },
  { name: "object", value: { message: "not an Error", code: 7 } },
  { name: "array", value: ["failed", null, [1, true], { reason: "bad" }] },
  { name: "empty-array", value: [] },
  { name: "fixed-number", value: 1000000 }, { name: "large-fixed-number", value: 1e20 },
  { name: "exponent-number", value: 1e21 }, { name: "small-fixed-number", value: 1e-6 },
  { name: "small-exponent-number", value: 1e-7 },
  { name: "negative-zero", kind: "number", value: "-0" },
  { name: "nan", kind: "number", value: "NaN" },
  { name: "infinity", kind: "number", value: "Infinity" },
];
const cases = [];
for (const spec of values) {
  const failure = spec.kind === "error" ? new Error(spec.value as string) : spec.kind === "number" ? Number(spec.value) : spec.value;
  const fail = () => { throw failure; };
  const agent = new Agent({ initialState: { model }, streamFn: fail });
  const events: unknown[] = [];
  agent.subscribe(event => { events.push(JSON.parse(JSON.stringify(event))); });
  await agent.prompt("go");
  const tools: Record<string, unknown> = {};
  for (const stage of ["prepare", "before", "execute", "after"]) {
    const tool = { name: "tool", label: "tool", description: "", parameters: { type: "object" }, execute: () => ({ content: [], details: {} }) };
    if (stage === "prepare") (tool as any).prepareArguments = fail;
    if (stage === "execute") tool.execute = fail;
    const call = { type: "toolCall", name: "tool", id: "call", arguments: {} };
    tools[stage] = await runToolCall(call, {
      tools: [tool], assistantMessage: { role: "assistant", content: [call] }, context: { messages: [], tools: [tool] },
      ...(stage === "before" ? { beforeToolCall: fail } : {}),
      ...(stage === "after" ? { afterToolCall: fail } : {}),
    } as any);
  }
  cases.push({ input: spec, expected: { events, state: { ...agent.state, pendingToolCalls: [...agent.state.pendingToolCalls] }, tools } });
}
const updates = [];
for (const mode of ["sync", "caught", "async"]) {
  const failure = new Error("update failed");
  let continued = false, caught = false, after = false;
  const tool = { name: "tool", label: "tool", description: "", parameters: { type: "object" }, execute: (_id: any, _args: any, _signal: any, update: any) => {
    if (mode === "caught") {
      try { update({ content: [], details: {} }); } catch (error) { caught = error === failure; }
    } else update({ content: [], details: {} });
    continued = true;
    return { content: [], details: {} };
  } };
  const call = { type: "toolCall", id: "call", name: "tool", arguments: {} };
  let outcome: unknown, error: string | undefined, sameError: boolean | undefined;
  try {
    outcome = await runToolCall(call, { tools: [tool], assistantMessage: { role: "assistant", content: [call] }, context: { messages: [], tools: [tool] },
      onUpdate: mode === "async" ? () => Promise.reject(failure) : () => { throw failure; },
      afterToolCall: () => { after = true; },
    } as any);
  } catch (value) { error = (value as Error).message; sameError = value === failure; }
  updates.push({ mode, expected: { outcome, error, sameError, continued, caught, after } });
}
const pendingUpdates = [];
for (const mode of ["success", "tool-error", "update-error", "tool-and-update-error", "already-rejected-order", "late-update-error", "late-tool-and-update-error"]) {
  const firstError = new Error("first update failed"), secondError = new Error("second update failed");
  const toolError = new Error("tool failed");
  let release!: () => void, rejectFirst!: (value: unknown) => void;
  const pending = new Promise<void>((resolve, reject) => { release = resolve; rejectFirst = reject; });
  const late = mode.startsWith("late-");
  let rejectSecond!: (value: unknown) => void;
  const second = new Promise<void>((_resolve, reject) => { rejectSecond = reject; });
  // A rejection can precede Promise.all registration, as when execute awaits
  // another task after scheduling updates. Avoid a fixture-level unhandled event.
  pending.catch(() => {});
  let after = false, settled = false, outcome: unknown, error: string | undefined;
  const tool = { name: "tool", label: "tool", description: "", parameters: { type: "object" }, execute: (_id: any, _args: any, _signal: any, update: any) => {
    update({ details: { id: "first" } });
    if (late || ["update-error", "tool-and-update-error", "already-rejected-order"].includes(mode)) update({ details: { id: "second" } });
    if (mode === "already-rejected-order") rejectFirst(firstError);
    if (["tool-error", "tool-and-update-error", "late-tool-and-update-error"].includes(mode)) throw toolError;
    return { content: [], details: {} };
  } };
  const call = { type: "toolCall", id: "call", name: "tool", arguments: {} };
  const run = runToolCall(call, { tools: [tool], assistantMessage: { role: "assistant", content: [call] }, context: { messages: [], tools: [tool] },
    onUpdate: (value: any) => value.details.id === "first" ? pending : late ? second : Promise.reject(secondError),
    afterToolCall: () => { after = true; },
  } as any).then(value => { outcome = value; settled = true; }, failure => { error = failure.message; settled = true; });
  await new Promise(resolve => setTimeout(resolve, 0));
  let beforeRejection: unknown;
  if (late) {
    beforeRejection = { settled, after };
    rejectSecond(secondError);
    await new Promise(resolve => setTimeout(resolve, 0));
  }
  const beforeRelease = { settled, after };
  release();
  await run;
  pendingUpdates.push({ mode, expected: { beforeRejection, beforeRelease, outcome, error, after } });
}
const destination = new URL("pi-failures.json", import.meta.url);
const output = JSON.stringify({ upstreamCommit: manifest.commit, model, cases, updates, pendingUpdates }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi failure fixtures changed; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} Agent/tool failure values, ${updates.length} update failures and ${pendingUpdates.length} pending-update cases against Pi ${manifest.commit}`);
