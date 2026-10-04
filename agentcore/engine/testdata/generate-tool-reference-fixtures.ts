import {readFileSync,writeFileSync} from "node:fs";
import {root,aiRoot,manifest} from "./oracle.ts";
const {runAgentLoop,runToolCall} = await import(new URL("upstream/packages/agent/src/agent-loop.ts",root).pathname);
const {AssistantMessageEventStream} = await import(new URL("utils/event-stream.ts",aiRoot).pathname);
const clone = (value: any) => JSON.parse(JSON.stringify(value));
Date.now=()=>1000;
const cases=[];
for(const mode of ["sequential","parallel","programmatic"]) for(const phase of ["start","before","before_block","before_failure","before_replace","before_slot","before_grow","execute","update","after","after_replace","after_failure","after_slot","after_grow","end","message_start","next_start","next_before","next_after","next_result"]) {
 if(mode==="programmatic" && (["start","end","message_start"].includes(phase)||phase.startsWith("next_"))) continue;
 const input={mode,phase};
 const events:any[]=[],hooks:any[]=[],executed:any[]=[],updates:any[]=[];
 let assistant:any={role:"assistant",content:[{type:"toolCall",id:"call",name:"echo",arguments:{value:"original"}}],api:"test",provider:"test",model:"test",usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:"toolUse",timestamp:1};
 if(phase.startsWith("next_")) assistant.content.push({type:"toolCall",id:"second",name:"echo",arguments:{value:"second"}});
 let retained=assistant.content[0];
 let firstDone!:()=>void;
 const firstEnded=new Promise<void>(resolve=>{firstDone=resolve;});
 const mutate=()=>Object.assign(retained,{id:"changed",name:"renamed",arguments:{value:"replacement"}});
 const editContent=(at:string)=>{
  if(phase===at+"_slot") assistant.content[0]={type:"toolCall",id:"slot",name:"renamed",arguments:{value:"slot"}};
  if(phase===at+"_grow") for(let i=0;i<8;i++) assistant.content.push({type:"text",text:"padding"});
 };
 const result={content:[{type:"text",text:"result"}],details:{},terminate:true};
 const tools=["echo","renamed"].map(name=>({name,label:name,description:name,parameters:{type:"object",properties:{value:{type:"string"}},required:["value"]},execute:async(id:string,args:any,_signal:any,update:any)=>{
  if(id==="second" && mode==="parallel") await firstEnded;
  executed.push(clone({tool:name,id,args}));
  if(phase==="execute") mutate();
  await update?.({content:[{type:"text",text:"partial"}],details:{}});
  return clone(result);
 }}));
 const hooksConfig={
  beforeToolCall:async(value:any)=>{
   hooks.push({hook:"before",identity:value.toolCall===value.assistantMessage.content[0],call:clone(value.toolCall),args:clone(value.args)});
   if(value.toolCall.id!=="second") retained=value.toolCall;
   if(phase==="next_before" && value.toolCall.id==="second") mutate();
   editContent("before");
   if(phase.startsWith("before")) mutate();
   if(phase==="before_replace") value.toolCall={type:"toolCall",id:"ignored",name:"ignored",arguments:{}};
   if(phase==="before_failure") throw new Error("before failed");
   if(phase==="before_block") return {block:true,reason:"blocked",terminate:true};
  },
  afterToolCall:async(value:any)=>{
   hooks.push({hook:"after",identity:value.toolCall===value.assistantMessage.content[0],call:clone(value.toolCall),args:clone(value.args)});
   editContent("after");
   if(phase.startsWith("after")) mutate();
   if(phase==="next_after" && value.toolCall.id==="second") mutate();
   if(phase==="after_replace") value.toolCall={type:"toolCall",id:"ignored",name:"ignored",arguments:{}};
   if(phase==="after_failure") throw new Error("after failed");
  },
 };
 let outcome:any,messages:any;
 if(mode==="programmatic") outcome=await runToolCall(retained,{tools,assistantMessage:assistant,context:{messages:[assistant],tools},...hooksConfig,onUpdate:async(partial:any)=>{updates.push(clone(partial));if(phase==="update") mutate();}});
 else {
  const stream=async()=>{const s=new AssistantMessageEventStream();s.push({type:"done",reason:"toolUse",message:assistant});return s;};
  messages=await runAgentLoop([{role:"user",content:"go",timestamp:0}],{messages:[],tools},{model:{id:"test",api:"test",provider:"test"},toolExecution:mode,convertToLlm:(m:any)=>m,...hooksConfig,finishTurn:()=>({action:"end"})},async(event:any)=>{
   events.push(clone(event));
   if(event.type==="message_end" && event.message.role==="assistant") {assistant=event.message;retained=assistant.content[0];}
   if((phase==="start" && event.type==="tool_execution_start") || (phase==="update" && event.type==="tool_execution_update") || (phase==="end" && event.type==="tool_execution_end") || (phase==="message_start" && event.type==="message_start" && event.message.role==="toolResult")) mutate();
   if((phase==="next_start" && event.type==="tool_execution_start" && event.toolCallId==="second") || (phase==="next_result" && event.type==="message_start" && event.message.role==="toolResult" && event.message.toolCallId==="second")) mutate();
   if(event.type==="tool_execution_end" && event.toolCallId!=="second") firstDone();
  },undefined,stream);
 }
 cases.push({input,expected:{events,hooks,executed,updates,assistant,retained,...(outcome?{outcome}:{}),...(messages?{messages}:{})}});
}
const serialized=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination=new URL("pi-tool-references.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==serialized)throw new Error("Tool reference fixtures changed");}
else writeFileSync(destination,serialized);
console.log(`Verified ${cases.length} tool-reference cases against Pi ${manifest.commit}`);
