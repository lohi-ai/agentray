import {readFileSync,writeFileSync} from "node:fs";
import {root,aiRoot,manifest} from "./oracle.ts";
const {runAgentLoop,runToolCall}=await import(new URL("upstream/packages/agent/src/agent-loop.ts",root).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
const clone=(value:any)=>JSON.parse(JSON.stringify(value));
const result=(text:string)=>({content:[{type:"text",text}],details:{},terminate:true});
Date.now=()=>1000;
const cases=[];
for(const mode of ["sequential","parallel","programmatic"]) for(const phase of ["plain","after_mutate","after_override","after_same_override","after_replace","after_failure","update_mutate","update_returned","update_later","update_nil","nil_result","end_mutate","end_original"]) {
 if(mode==="programmatic" && phase.startsWith("end_"))continue;
 const input={mode,phase};
 const events:any[]=[],hooks:any[]=[],updateSnapshots:any[]=[],liveUpdates:any[]=[],identities:any[]=[];
 const original=result("original"),partial=phase==="update_nil"?null:result("partial");
 let produced:any=original,endResult:any,afterResult:any;
 const mutate=(value:any)=>{value.content=[{type:"text",text:"mutated"}];value.details={changed:true};};
 const onUpdate=(value:any)=>{
  identities.push({update:value===partial});
  updateSnapshots.push(clone(value));liveUpdates.push(value);
  if(phase==="update_mutate")mutate(value);
 };
 const tool={name:"echo",label:"echo",description:"echo",parameters:{type:"object"},execute:async(_id:any,_args:any,_signal:any,update:any)=>{
  await update(partial);
  if(phase==="update_later")mutate(partial);
  produced=phase==="update_returned"?partial:phase==="nil_result"?null:original;
  return produced;
 }};
 const after=async(value:any)=>{
  afterResult=value.result;
  identities.push({after:value.result===produced});
  hooks.push(clone({result:value.result,isError:value.isError}));
  if(["after_mutate","after_failure"].includes(phase))mutate(value.result);
  if(phase==="after_failure")throw new Error("after failed");
  if(phase==="after_override")return {content:[{type:"text",text:"override"}]};
  if(phase==="after_same_override")return value.result;
  if(phase==="after_replace")value.result=result("ignored");
 };
 const call={type:"toolCall",id:"call",name:"echo",arguments:{}};
 const assistant={role:"assistant",content:[call],api:"test",provider:"test",model:"test",usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:"toolUse",timestamp:1};
 let outcome:any,messages:any;
 if(mode==="programmatic"){
  outcome=await runToolCall(call,{tools:[tool],assistantMessage:assistant,context:{messages:[],tools:[tool]},onUpdate,afterToolCall:after});
  identities.push({outcome:outcome.result===produced});
 } else {
  messages=await runAgentLoop([{role:"user",content:"go",timestamp:0}],{messages:[],tools:[tool]},{model:{id:"test",api:"test",provider:"test"},toolExecution:mode,convertToLlm:(m:any)=>m,afterToolCall:after,finishTurn:()=>({action:"end"})},async(event:any)=>{
   events.push(clone(event));
   if(event.type==="tool_execution_update")onUpdate(event.partialResult);
   if(event.type==="tool_execution_end"){
    endResult=event.result;
    identities.push({end:event.result===produced});
    if(phase==="end_mutate")mutate(event.result);
    if(phase==="end_original")mutate(produced);
   }
  },undefined,async()=>{const s=new AssistantMessageEventStream();s.push({type:"done",reason:"toolUse",message:assistant});return s;});
 }
 if(produced)produced.details={post:true};
 cases.push({input,expected:{events,hooks,updateSnapshots,liveUpdates,identities,original,partial,produced,afterResult,...(endResult?{endResult}:{}),...(outcome?{outcome}:{}),...(messages?{messages}:{})}});
}
const serialized=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination=new URL("pi-result-references.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==serialized)throw new Error("Result reference fixtures changed");}
else writeFileSync(destination,serialized);
console.log(`Verified ${cases.length} result-reference cases against Pi ${manifest.commit}`);
