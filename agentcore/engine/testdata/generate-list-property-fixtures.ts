import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
const names = ["-1", "01", "4294967295", "a", "2", "1", "__proto__", "constructor", "-0", "1e0", "0002"];
const cases = [];
for (const phase of ["plain", "overwrite", "index_overwrite", "delete_reinsert", "delete_index", "shrink", "grow", "clone", "clone_default", "clone_object", "clone_undefined", "decode_grow", "decode_delete"]) {
 let list: any = ["initial"];
 const define = (key: string, value: string) => Object.defineProperty(list, key, {value, writable: true, enumerable: true, configurable: true});
 for (const name of names) define(name, name);
 if (phase === "overwrite") define("a", "edited");
 if (phase === "index_overwrite") define("1", "edited");
 if (phase === "delete_reinsert") {delete list["-1"]; define("-1", "edited");}
 if (phase === "delete_index" || phase === "decode_delete") delete list[1];
 if (phase === "shrink") list.length = 1;
 if (phase === "grow" || phase === "decode_grow") list.length = 5;
 if (phase === "clone_default") delete list.constructor;
 if (phase === "clone_object") define("constructor", {} as any);
 if (phase === "clone_undefined") define("constructor", undefined as any);
 if (phase.startsWith("clone")) {
  try {list = list.slice();}
  catch (error) {cases.push({phase, expected: {error: (error as Error).message}}); continue;}
 }
 if (phase.startsWith("decode_")) list = JSON.parse(JSON.stringify(list));
 const lookups = Object.fromEntries(["0", "3", ...names, "absent"].map(key => [key, {found: Object.hasOwn(list, key), value: Object.hasOwn(list, key) ? list[key] : null}]));
 cases.push({phase, expected: {length: list.length, keys: Object.keys(list), indexKeys: Object.keys(list).filter(key => String(Number(key)) === key && Number(key) >= 0 && Number(key) < 4294967295).map(Number), wire: JSON.stringify(list), lookups}});
}
Date.now = () => 1000;
const agents = [];
for (const subject of ["messages", "tools"]) {
 const events: string[] = [];
 let requests = 0;
 const agent = new Agent({initialState: {model: {id: "test", api: "test", provider: "test"}}, streamFn: async () => {
  requests++;
  const message = {role: "assistant", content: [{type: "text", text: "done"}], api: "test", provider: "test", model: "test", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "stop", timestamp: 2};
  const stream = new AssistantMessageEventStream(); stream.push({type: "done", reason: "stop", message}); return stream;
 }} as any);
 agent.subscribe(event => {events.push(event.type);});
 const selected = (agent.state as any)[subject];
 Object.defineProperty(selected, "constructor", {value: null, enumerable: true, configurable: true});
 const observe = () => ({events: [...events], requests, streaming: agent.state.isStreaming, error: agent.state.errorMessage ?? null, messages: Array.from(agent.state.messages, (m: any) => ({role: m.role, timestamp: m.timestamp, error: m.errorMessage ?? null}))});
 await agent.prompt({role: "user", content: "go", timestamp: 1});
 await agent.waitForIdle();
 const failed = observe();
 delete selected.constructor;
 await agent.prompt({role: "user", content: "go", timestamp: 1});
 await agent.waitForIdle();
 agents.push({subject, expected: {failed, recovered: observe()}});
}
const output = JSON.stringify({upstreamCommit: manifest.commit, names, cases, agents}, null, 2) + "\n";
const destination = new URL("pi-list-properties.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination, "utf8") !== output) throw new Error("Pi array-property fixtures differ");
} else writeFileSync(destination, output);
console.log(`Verified ${cases.length} array-property and ${agents.length} admission-recovery cases in the pinned Pi oracle runtime`);
