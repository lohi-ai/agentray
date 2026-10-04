// Verify that the agent observes live JS numbers without a JSON round trip.
import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop} = await import(new URL("upstream/packages/agent/src/agent-loop.ts", root).pathname);
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts", root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts", aiRoot).pathname);
Date.now = () => 1000;
const classify = (n: number) => Number.isNaN(n) ? "NaN" : n === Infinity ? "Infinity" : n === -Infinity ? "-Infinity" : Object.is(n, -0) ? "-0" : String(n);
const usage = (n: number) => ({input:n,output:n,cacheRead:n,cacheWrite:n,cacheWrite1h:n,reasoning:n,totalTokens:n,cost:{input:n,output:n,cacheRead:n,cacheWrite:n,total:n}});
const describe = (message: any) => ({stopReason: message.stopReason, wire: JSON.stringify(message.usage), usage: Object.fromEntries(Object.entries(message.usage ?? {}).map(([key, value]) => [key, key === "cost" ? Object.fromEntries(Object.entries(value as object).map(([key, value]) => [key, classify(value as number)])) : classify(value as number)]))});
const cases = [];
for (const mode of ["loop", "agent"]) for (const stage of ["updates", "terminal", "ignored"]) {
  for (const kind of ["NaN", "Infinity", "-Infinity", "-0"]) {
    const n = kind === "NaN" ? NaN : Number(kind);
    const message: any = {role:"assistant",content:[{type:"text",text:"done"}],api:"test",provider:"test",model:"test",usage:usage(n),stopReason:"stop",timestamp:1};
    const final = stage === "ignored" ? {...message, usage:usage(0)} : message;
    const stream = async () => {
      const s = new AssistantMessageEventStream();
      if (stage === "updates") s.push({type:"start",partial:message});
      if (stage !== "terminal") s.push({type:"text_delta",contentIndex:0,delta:"done",partial:message});
      if (stage === "ignored") s.push({type:"vendor",partial:message} as any);
      s.push({type:"done",reason:"stop",message:final});
      return s;
    };
    const events: any[] = [], completed: any[] = [];
    const emit = (event: any) => {
      events.push({type:event.type,...(event.message?.role === "assistant" ? {message:describe(event.message)} : {})});
      if (event.type === "agent_end") completed.push(...event.messages.filter((m: any) => m.role === "assistant").map(describe));
    };
    const model = {id:"test",api:"test",provider:"test"};
    const prompt = {role:"user",content:"go",timestamp:0};
    if (mode === "loop") await runAgentLoop([prompt],{messages:[],tools:[]},{model,convertToLlm:(m: any)=>m},emit,undefined,stream);
    else {const agent = new Agent({initialState:{model},streamFn:stream});agent.subscribe(emit);await agent.prompt(prompt);}
    cases.push({input:{mode,stage,kind},expected:{events,completed}});
  }
}
const output = JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination = new URL("pi-stream-numbers.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Pi stream number fixtures differ");}
else writeFileSync(destination,output);
console.log(`Verified ${cases.length} stream number cases against Pi ${manifest.commit}`);
