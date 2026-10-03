// The unchanged public Pi wrappers are the oracle, including rejected producers.
import {readFileSync,writeFileSync} from "node:fs";
import {root,aiRoot,manifest} from "./oracle.ts";
const {agentLoop,agentLoopContinue}=await import(new URL("upstream/packages/agent/src/agent-loop.ts",root).pathname);
const {setDefaultStreamFn}=await import(new URL("upstream/packages/agent/src/stream-fn.ts",root).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",api:"test",provider:"test"};
const user={role:"user",content:"go",timestamp:100};
const assistant={role:"assistant",content:[{type:"text",text:"done"}],timestamp:100,api:"test",provider:"test",model:"test",stopReason:"stop",usage:{input:1,output:2,cacheRead:0,cacheWrite:0,totalTokens:3,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}}};
const inputs=[
 {name:"prompt"},{name:"continue",resume:true},{name:"empty continuation",resume:true,empty:true},{name:"assistant continuation",resume:true,assistantTail:true},
 {name:"custom continuation",resume:true,custom:true},{name:"provider returned error",stopReason:"error"},{name:"provider returned aborted",stopReason:"aborted"},
 {name:"provider rejection",providerFailure:"provider failed"},{name:"transform rejection",transformFailure:"transform failed"},{name:"missing default",missing:true},{name:"configured default",useDefault:true},
 {name:"result without terminal event",omitTerminal:true},{name:"tool update and termination",tool:true},
];
const clone=(value:any)=>JSON.parse(JSON.stringify(value));
const delay=()=>new Promise(resolve=>setTimeout(resolve,5));
let failure:Error|undefined;
process.on("unhandledRejection",(error)=>{failure=error as Error});
const cases=[];
for(const input of inputs){
 failure=undefined;
 const events:any[]=[],requests:any[]=[];
 const config:any={model,convertToLlm:(messages:any[])=>messages.map(m=>m.role==="custom"?{...m,role:"user"}:m)};
 if(input.transformFailure)config.transformContext=()=>{throw new Error(input.transformFailure)};
 const tools=input.tool?[{name:"echo",label:"Echo",description:"Echo",parameters:{type:"object"},execute:async(_id:any,_args:any,_signal:any,update:any)=>{update({content:[{type:"text",text:"working"}]});return {content:[{type:"text",text:"done"}],terminate:true}}}]:[];
 const provider=(model:any,context:any,options:any)=>{
  const {signal,...serializable}=options;
  requests.push(clone({model,context,options:serializable}));
  if(input.providerFailure)throw new Error(input.providerFailure);
  const message=clone(assistant);
  if(input.stopReason){message.stopReason=input.stopReason;message.errorMessage="failed"}
  if(input.tool){message.stopReason="toolUse";message.content=[{type:"toolCall",id:"c1",name:"echo",arguments:{}}]}
  const stream=new AssistantMessageEventStream();
  if(!input.omitTerminal){
   stream.push({type:"start",partial:message});
   stream.push(message.stopReason==="error"||message.stopReason==="aborted"?{type:"error",reason:message.stopReason,error:message}:{type:"done",reason:message.stopReason,message});
  }
  stream.end(message);return stream;
 };
 setDefaultStreamFn(input.useDefault?provider:undefined);
 const messages=input.empty?[]:input.assistantTail?[clone(assistant)]:[{...user,role:input.custom?"custom":"user"}];
 let stream:any,syncError:string|null=null;
 try{stream=input.resume?agentLoopContinue({messages,tools},config,undefined,input.useDefault||input.missing?undefined:provider):agentLoop([clone(user)],{messages:[],tools},config,undefined,input.useDefault||input.missing?undefined:provider)}catch(error){syncError=(error as Error).message}
 let result:any=null,resultPending=false,queuePending=false;
 if(stream){
  const rejected=!!(input.providerFailure||input.transformFailure||input.missing);
  if(rejected){for(let i=0;i<100&&!failure;i++)await delay();if(!failure)throw new Error("producer rejection was not observed")}
  else result=clone(await stream.result());
  const iterator=stream[Symbol.asyncIterator]();
  while(true){
   const item:any=await Promise.race([iterator.next(),delay().then(()=>({pending:true}))]);
   if(item.pending){queuePending=true;break}
   if(item.done)break;
   events.push(clone(item.value));
  }
  if(rejected){resultPending=await Promise.race([stream.result().then(()=>false),delay().then(()=>true)]);stream.end([])}
 }
 cases.push({input,expected:{events,requests,result,syncError,producerError:failure?.message??null,resultPending,queuePending}});
}
setDefaultStreamFn(undefined);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,assistant,now:100,cases},null,2)+"\n";
const file=new URL("loop-stream.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Loop stream fixtures changed; regenerate and review")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} public loop stream cases against Pi ${manifest.commit}`);
