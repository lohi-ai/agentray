import {readFileSync, writeFileSync} from "node:fs";
import {root, aiRoot, manifest} from "./oracle.ts";
const {runAgentLoop} = await import(new URL("upstream/packages/agent/src/agent-loop.ts",root).pathname);
const {Agent} = await import(new URL("upstream/packages/agent/src/agent.ts",root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts",aiRoot).pathname);
const clone = (value: any) => JSON.parse(JSON.stringify(value));
Date.now = () => 1000;
const model = {id:"test",api:"test",provider:"test"};
const usage = {input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}};
const cases = [];
for (const mode of ["loop","agent"]) for (const role of ["user","assistant","toolResult"]) for (const stopReason of role === "assistant" ? ["stop","error","aborted"] : ["stop"]) {
 const variants = ["finish","turn_end","next","request","agent_end","transform","convert","key","steering"].map(phase => ({phase, through:"retained"}));
 for (const through of ["context","newMessages",role === "assistant" ? "message" : role === "toolResult" ? "toolResults" : "context"]) {
  if (!variants.some(v => v.phase === "finish" && v.through === through)) variants.push({phase:"finish",through});
 }
 for (const {phase,through} of variants) {
  if(stopReason !== "stop" && !["finish","turn_end","agent_end"].includes(phase)) continue;
  const input = {mode,role,phase,through,stopReason};
  const events: any[] = [], requests: any[] = [], identities: any[] = [];
  let retained: any, mutated = false, turns = 0, requestCount = 0, responseCount = 0;
  const mutate = (at: string, turn?: any) => {
   if (at !== phase || mutated || !retained) return;
   let target = retained;
   if (through === "context") target = turn.context.messages.find((m: any) => m.role === role);
   if (through === "newMessages") target = turn.newMessages.find((m: any) => m.role === role);
   if (through === "message") target = turn.message;
   if (through === "toolResults") target = turn.toolResults[0];
   target.timestamp = 77;
   mutated = true;
  };
  const tools = role === "toolResult" ? [{name:"echo",label:"echo",description:"echo",parameters:{type:"object"},execute:async()=>({content:[{type:"text",text:"result"}],details:{}})}] : [];
  const config = {
   model,
   transformContext:async(messages: any[])=>{mutate("transform");return messages;},
   convertToLlm:(messages: any[])=>{mutate("convert");return messages;},
   getApiKey:()=>{mutate("key");return "test";},
   getSteeringMessages:async()=>{mutate("steering");return [];},
   finishTurn:async (turn: any) => {
    if (turns++ === 0) {
     identities.push({context:turn.context.messages.find((m: any)=>m.role===role)===retained,newMessages:turn.newMessages.find((m: any)=>m.role===role)===retained,...(role === "assistant" ? {message:turn.message===retained} : role === "toolResult" ? {toolResults:turn.toolResults[0]===retained} : {})});
     mutate("finish",turn);
     return {action:"continue"};
    }
    return {action:"end"};
   },
   prepareNextTurn:async ()=>{mutate("next");},
   prepareRequest:async ()=>{if(requestCount++>0) mutate("request");},
  };
  const stream = async (_model: any, context: any) => {
   requests.push(clone(context));
   const tool = role === "toolResult" && responseCount++ === 0;
   const message = {role:"assistant",content:tool ? [{type:"toolCall",id:"call",name:"echo",arguments:{}}] : [{type:"text",text:"done"}],api:"test",provider:"test",model:"test",usage,stopReason:tool?"toolUse":stopReason,timestamp:100};
   const result = new AssistantMessageEventStream();
   if(["error","aborted"].includes(message.stopReason)) result.push({type:"error",reason:message.stopReason,error:message});
   else result.push({type:"done",reason:message.stopReason,message});
   return result;
  };
  let messages: any[] = [];
  const sink = async(event: any)=>{
   events.push(clone(event));
   if(event.type === "message_end" && event.message.role === role && !retained) retained=event.message;
   mutate(event.type);
   if(event.type === "agent_end") messages=event.messages;
  };
  const prompt = {role:"user",content:"hello",timestamp:1};
  let stateMessages: any[] | undefined;
  if(mode === "loop") messages = await runAgentLoop([prompt],{messages:[],tools},config,sink,undefined,stream);
  else {
   // Agent owns its steering callback, so exercise the same mutation in the
   // wrapper's public next-turn callback for that variant.
   const agent = new Agent({...config,initialState:{model,tools},streamFn:stream,prepareNextTurn:async()=>{mutate("next");mutate("steering");}});
   agent.subscribe(sink);
   await agent.prompt(prompt);
   stateMessages=agent.state.messages;
  }
  cases.push({input,expected:{events,requests,identities,messages,retained,prompt,...(stateMessages ? {stateMessages} : {})}});
 }
}
const serialized = JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination = new URL("pi-message-references.json",import.meta.url);
if(process.argv.includes("--check")) {
 if(readFileSync(destination,"utf8")!==serialized) throw new Error("Message reference fixtures changed");
} else writeFileSync(destination,serialized);
console.log(`Verified ${cases.length} message-reference cases against Pi ${manifest.commit}`);
