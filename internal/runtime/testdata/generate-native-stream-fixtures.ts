// Migration oracle: run the existing worker adapter unchanged, with pinned Pi.
// No worker or JS process is used by the Go implementation/tests.
import { readFileSync, writeFileSync } from "node:fs";
import { PiWorker, InMemoryTelemetryContext, manifest } from "./worker-oracle.ts";
const message=(text:string,stopReason="stop")=>({role:"assistant",content:[{type:"text",text}],stopReason,timestamp:100,api:"test",provider:"test",model:"test",usage:{input:1,output:1,cacheRead:0,cacheWrite:0,totalTokens:2,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}}});
const partial=message("partial","pending"),final=message("done");
const inputs:any[]=[
 {name:"final-only",result:final},
 {name:"partial-and-final",events:[{type:"start",partial},{type:"text_delta",contentIndex:0,delta:"partial",partial}],result:final},
 {name:"terminal-wins-returned-message",events:[{type:"start",partial},{type:"done",reason:"stop",message:final}],result:message("ignored")},
 {name:"terminal-wins-null",events:[{type:"done",reason:"stop",message:final}],result:null},
 {name:"provider-error",result:{...message("","error"),errorMessage:"provider rejected"}},
 {name:"provider-aborted",result:{...message("","aborted"),errorMessage:"aborted"}},
 {name:"unfinished-message",result:partial},
 {name:"wrong-role",result:{role:"user",content:"bad",timestamp:100}},
 {name:"missing-message"},
 {name:"scalar-message",result:"not an assistant"},
 {name:"null-message",result:null},
 {name:"callback-failure",failure:"provider failed"},
 {name:"named-callback-failure",failure:"invalid request",failureName:"TypeError"},
 {name:"callback-failure-after-progress",events:[{type:"start",partial}],failure:"provider failed after progress"},
 {name:"callback-failure-after-terminal",events:[{type:"start",partial},{type:"done",reason:"stop",message:final}],failure:"provider failed after terminal",settleAfterIteration:true},
 {name:"ignore-post-terminal-progress",events:[{type:"done",reason:"stop",message:final},{type:"text_delta",contentIndex:0,delta:"ignored",partial}],result:final},
 {name:"options-filter",options:{apiKey:"callback-credential",maxTokens:10,temperature:0,signal:true,telemetryContext:true,onPayload:true,onResponse:true,onProviderStreamEvent:true,metadata:{task:"keep"}},result:final},
];
const model={id:"test",api:"test",provider:"test"};
const context={messages:[{role:"user",content:"test",timestamp:100}]};
const cases=[];
for(const input of inputs){
 const recorder=new InMemoryTelemetryContext();
 const requests:any[]=[];
 let settle:(()=>void)|undefined;
 const worker=new PiWorker((frame:any)=>{
  if(frame.kind!=="callback"||frame.method!=="stream")throw new Error("unexpected callback");
  requests.push(JSON.parse(JSON.stringify(frame.params)));
  queueMicrotask(()=>{
   for(const event of input.events??[])worker.receive({kind:"progress",id:frame.id,value:JSON.parse(JSON.stringify(event))});
   const finish=()=>worker.receive({kind:"result",id:frame.id,...(input.failure?{error:{name:input.failureName??"Error",message:input.failure}}:{value:input.result})});
   if(input.settleAfterIteration)settle=finish;else finish();
  });
 });
 (worker as any).activeTelemetry=recorder;
 const stream=(worker as any).stream(model,context,{...input.options,...(input.options?.signal?{signal:new AbortController().signal}:{})});
 const events:any[]=[];
 let iterationError:string|null=null,resultError:string|null=null,result:any=null;
 try{for await(const event of stream)events.push(event)}catch(error){iterationError=(error as Error).message}
 settle?.();
 try{result=await stream.result()}catch(error){resultError=(error as Error).message}
 cases.push({input,expected:JSON.parse(JSON.stringify({requests,events,result,iterationError,resultError,spans:recorder.getSpans()}))});
 worker.close();
}
const output=JSON.stringify({upstreamCommit:manifest.commit,model,context,cases},null,2)+"\n";
const destination=new URL("native-stream.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Native stream migration fixtures changed; regenerate and review")}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} callback stream cases against the existing worker and Pi ${manifest.commit}`);
