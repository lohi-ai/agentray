import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",api:"anthropic-messages",provider:"anthropic",baseUrl:"https://unused.test",reasoning:true,input:["text"],maxTokens:8192,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
const success='event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}\n\n';
const inputs:any[]=[
 {name:"custom client without credentials"},
 {name:"OAuth key does not change identity",options:{apiKey:"sk-ant-oat-fixture"},context:{messages:[],tools:[{name:"read",description:"read",parameters:{type:"object",properties:{}}}]}},
 {name:"invalid builtin headers ignored",model:{headers:{"bad header":"value"}}},
 {name:"fractional timeout forwarded",options:{timeoutMs:1.5}},
 {name:"negative timeout forwarded",options:{timeoutMs:-1}},
 {name:"null timeout forwarded",options:{timeoutMs:null}},
 {name:"payload replacement stream forced",replacement:{model:"other",stream:false,betas:["custom"],user_profile_id:"profile"}},
 {name:"payload null replacement",replacement:null},
 {name:"SDK output format transform owned by client",replacement:{output_format:{type:"json_schema"},output_config:{format:{type:"json_schema"}}}},
 {name:"payload callback fails",fail:"payload"},
 {name:"response callback fails",fail:"response"},
 {name:"event callback fails",fail:"event"},
 {name:"client ordinary error",failures:[{message:"client failed"}]},
 {name:"ordinary error not retried",options:{maxRetries:2},failures:[{message:"ordinary failed"}]},
 {name:"provider error retried",options:{maxRetries:2},failures:[{message:"429 wait",status:429,headers:{"retry-after-ms":"0"}}]},
 {name:"provider retry directive denied",options:{maxRetries:2},failures:[{message:"500 refused",status:500,headers:{"x-should-retry":"false"}}]},
 {name:"provider retry delay capped",options:{maxRetries:2,maxRetryDelayMs:50},failures:[{message:"429 wait",status:429,headers:{"retry-after-ms":"100"}}]},
 {name:"returned status is owned by client",status:400},
 {name:"no body",status:204},
 {name:"empty body missing stop",body:""},
];
const cases=[];
for(const input of inputs){
 const calls:any[]=[];const phases:string[]=[];
 const client={beta:{messages:{create:(params:any,options:any)=>{
  phases.push("request");calls.push({params:JSON.parse(JSON.stringify(params)),options:{...options,signal:undefined}});
  return {asResponse:async()=>{
   const failure=input.failures?.[calls.length-1];
   if(failure){const error:any=new Error(failure.message);if(failure.status!==undefined){error.status=failure.status;error.headers=new Headers(failure.headers)}throw error}
   return new Response(input.status===204?null:(input.body??success),{status:input.status??200,headers:{"content-type":"text/event-stream","x-client":"custom"}});
  }};
 }}}};
 const resultStream=stream({...model,...input.model},normalizeContext(input.context??{messages:[]}),{...input.options,client,
  fetch:async()=>{throw new Error("builtin fetch called")},
  onPayload:()=>{phases.push("payload");if(input.fail==="payload")throw new Error("payload failed");return input.replacement},
  onResponse:()=>{phases.push("response");if(input.fail==="response")throw new Error("response failed")},
  onProviderStreamEvent:()=>{phases.push("event");if(input.fail==="event")throw new Error("event failed")},
 });
 const result=await resultStream.result();const events=[];for await(const event of resultStream)events.push(event.type);
 cases.push({input,expected:{calls,phases,events,result}});
}
const file=new URL("pi-anthropic-client.json",import.meta.url);const output=JSON.stringify({upstreamCommit:manifest.commit,model,success,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic client oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic injected-client cases`);
