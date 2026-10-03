import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
Date.now=()=>100;
const token="header."+btoa(JSON.stringify({"https://api.openai.com/auth":{chatgpt_account_id:"account"}}))+".signature";
const shared=JSON.parse(readFileSync(new URL("pi-responses-stream.json",import.meta.url),"utf8"));
const model={...shared.model,api:"openai-codex-responses",provider:"openai-codex",baseUrl:"https://chatgpt.com/backend-api"};
const tool=shared.tool;
const terminal=(extra:any={})=>({type:"response.done",response:{id:"r",status:"completed",output:[],...extra}});
const usage={input_tokens:20,output_tokens:8,total_tokens:28,input_tokens_details:{cached_tokens:4,cache_write_tokens:2}};
const inputs:any[]=[...shared.cases.map((c:any)=>c.input),
 ...[true,false,null,0,"true"].map(end_turn=>({name:`Codex end turn ${JSON.stringify(end_turn)}`,chunks:[terminal({end_turn})]})),
 ...["flex","priority","fast"].map(serviceTier=>({name:`Codex default response tier ${serviceTier}`,options:{serviceTier},chunks:[terminal({usage,service_tier:"default"})]})),
 {name:"Codex terminal stops later frames",chunks:[terminal({end_turn:true}),{type:"error",message:"must not run"}]},
 {name:"Codex error nested",chunks:[{type:"error",error:{code:"nested",message:"nested message"}}]},
 {name:"Codex error top level wins",chunks:[{type:"error",code:"top",message:"top message",error:{code:"nested",message:"nested message"}}]},
 {name:"Codex error empty top level",chunks:[{type:"error",code:"",message:"",error:{code:"nested",message:"nested message"}}]},
 {name:"Codex event without type ignored",chunks:[{message:"ignored"},terminal()]},
 {name:"Codex non-string type ignored",chunks:[{type:42},terminal()]},
];
const originalPush=AssistantMessageEventStream.prototype.push;
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model,compat:input.compat};
 const events:any[]=[];const live:any[]=[];const controller=new AbortController();
 AssistantMessageEventStream.prototype.push=function(event:any){events.push(JSON.parse(JSON.stringify(event)));live.push(event);if(input.abortAtEnd&&event.type==="text_end")controller.abort();return originalPush.call(this,event)};
 let received=0;
 const resultStream=stream(chosen,normalizeContext({messages:[],tools:[tool]}),{...input.options,
 apiKey:token,transport:"sse",signal:controller.signal,maxRetries:0,
 fetch:async()=>new Response(input.chunks.map((value:any)=>`data: ${JSON.stringify(value)}\n\n`).join("")+"data: [DONE]\n\n",{headers:{"content-type":"text/event-stream"}}),
 onProviderStreamEvent:()=>{if(received++===input.failBefore)throw new Error("fixture failure")},
 });
 const result=await resultStream.result();for await(const _ of resultStream){}
 for(const event of live){if(event.partial&&event.partial!==result)throw new Error("original lost partial identity")}
 cases.push({input,expected:{events,result}});
}
AssistantMessageEventStream.prototype.push=originalPush;
const file=new URL("pi-codex-stream.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,tool,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Codex stream oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex stream cases against Pi ${manifest.commit}`);
