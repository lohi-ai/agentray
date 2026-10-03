import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
for(const key of ["OPENAI_ORG_ID","OPENAI_PROJECT_ID","PI_CACHE_RETENTION","ANTHROPIC_CUSTOM_HEADERS","ANTHROPIC_FEDERATION_RULE_ID","ANTHROPIC_ORGANIZATION_ID","ANTHROPIC_IDENTITY_TOKEN_FILE"])delete process.env[key];
const model={id:"test",api:"anthropic-messages",provider:"anthropic",baseUrl:"https://example.test/v1",reasoning:false,input:["text","image"],maxTokens:8192,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2},headers:{"User-Agent":"fixture"}};
const chunks=[{type:"message_start",message:{id:"response",model:"test",usage:{input_tokens:10,output_tokens:1}}},{type:"content_block_start",index:0,content_block:{type:"text",text:""}},{type:"content_block_delta",index:0,delta:{type:"text_delta",text:"hello"}},{type:"content_block_stop",index:0},{type:"message_delta",delta:{stop_reason:"end_turn"}},{type:"message_stop"}];
const frame=(value:any)=>`event: ${value.type}\ndata: ${JSON.stringify(value)}\n\n`;
const success=chunks.map(frame).join("");
const tool={name:"read",description:"read",parameters:{type:"object",properties:{}}};
const inputs:any[]=[
 {name:"success"},
 {name:"missing api key",options:{apiKey:null}},
 {name:"empty key",options:{apiKey:""}},
 {name:"model header alone does not supply auth",model:{headers:{Authorization:"model-key"}},options:{apiKey:null}},
 {name:"authorization header auth",options:{apiKey:null,headers:{Authorization:"Bearer header-key"}}},
 {name:"x-api-key header auth",options:{apiKey:null,headers:{"x-api-key":"header-key"}}},
 {name:"gateway auth explicit omission",options:{apiKey:null,headers:{"CF-AIG-Authorization":"gateway",Authorization:null}}},
 {name:"gateway auth without omission rejected by SDK",options:{apiKey:null,headers:{"CF-AIG-Authorization":"gateway"}}},
 {name:"empty auth headers rejected",options:{apiKey:null,headers:{Authorization:" ","X-Api-Key":null}}},
 {name:"key auth deleted explicitly",options:{headers:{"X-Api-Key":null}}},
 {name:"key auth empty rejected by SDK",options:{headers:{"X-Api-Key":""}}},
 {name:"oauth identity",options:{apiKey:"sk-ant-oat01-fixture"},context:{messages:[],tools:[tool]}},
 {name:"oauth detection substring",options:{apiKey:"prefix-sk-ant-oat-token"}},
 {name:"oauth ignores affinity",model:{compat:{sendSessionAffinityHeaders:true}},options:{apiKey:"sk-ant-oat-token",sessionId:"session"}},
 {name:"oauth UA lower spelling override",options:{apiKey:"sk-ant-oat-token",headers:{"user-agent":"custom-oauth"}}},
 {name:"copilot bearer",model:{provider:"github-copilot"}},
 {name:"copilot token skips oauth identity",model:{provider:"github-copilot"},options:{apiKey:"sk-ant-oat-token"},context:{messages:[],tools:[tool]}},
 {name:"copilot vision dynamic",model:{provider:"github-copilot"},context:{messages:[{role:"toolResult",toolCallId:"c",toolName:"read",content:[{type:"image",mimeType:"image/png",data:"YQ=="}],isError:false,timestamp:1}]}},
 {name:"copilot dynamic override",model:{provider:"github-copilot",headers:{"X-Initiator":"model"}},options:{headers:{"X-Initiator":"option"}}},
 {name:"default no affinity",options:{sessionId:"session"}},
 {name:"openrouter affinity",model:{provider:"openrouter"},options:{sessionId:"session"}},
 {name:"generic affinity",model:{compat:{sendSessionAffinityHeaders:true}},options:{sessionId:"session"}},
 {name:"cache none no affinity",model:{provider:"openrouter"},options:{sessionId:"session",cacheRetention:"none"}},
 {name:"header overrides and deletions",model:{headers:{"User-Agent":"fixture","x-one":"model","x-delete":"remove"}},options:{headers:{"X-One":"option","x-delete":null,"content-type":"ignored","anthropic-version":"custom"}}},
 {name:"case collision ordered",model:{headers:{"User-Agent":"old","user-agent":"later"}},options:{headers:{"User-Agent":"override"}}},
 {name:"beta overrides",options:{headers:{"anthropic-beta":"custom, other"}}},
 {name:"beta deleted",options:{apiKey:"sk-ant-oat-token",headers:{"Anthropic-Beta":null}}},
 {name:"payload replacement stream forced",replacement:{model:"replaced",messages:[],stream:false}},
 {name:"payload replacement null",replacement:null},
 {name:"payload replacement scalar",replacement:7},
 {name:"payload replacement array",replacement:["a","b"]},
 {name:"payload replacement string",replacement:"abc"},
 {name:"payload header fields",replacement:{betas:["custom","another"],user_profile_id:"profile",workspace_id:"workspace",messages:[],max_tokens:1}},
 {name:"deprecated output format",replacement:{output_format:{type:"json_schema",schema:{type:"object"}},output_config:{effort:"high"}}},
 {name:"conflicting output formats",replacement:{output_format:{type:"json_schema"},output_config:{format:{type:"json_schema"}}}},
 {name:"payload callback fails",fail:"payload"},
 {name:"response callback fails",fail:"response"},
 {name:"chunk callback fails",fail:"chunk"},
 {name:"chunk callback mutates",mutate:true},
 {name:"http plain error",responses:[{status:403,body:"gateway denied"}]},
 {name:"http nested error",responses:[{status:400,body:'{"error":{"type":"invalid_request_error","message":"bad"},"request_id":"r"}'}]},
 {name:"http top level message",responses:[{status:400,body:'{"message":"bad"}'}]},
 {name:"http scalar error",responses:[{status:400,body:'"bad"'}]},
 {name:"http empty error",responses:[{status:401,body:""}]},
 {name:"http empty object",responses:[{status:400,body:"{}"}]},
 {name:"http no content",responses:[{status:204,body:null}]},
 {name:"retry 429",options:{maxRetries:2},responses:[{status:429,body:'{"error":{"message":"wait"}}',headers:{"retry-after-ms":"0"}},{body:success}]},
 {name:"retry directive allows 400",options:{maxRetries:1},responses:[{status:400,body:"retry",headers:{"x-should-retry":"true","retry-after-ms":"0"}},{body:success}]},
 {name:"retry directive denies 500",options:{maxRetries:2},responses:[{status:500,body:"no retry",headers:{"x-should-retry":"false"}}]},
 {name:"retry delay cap",options:{maxRetries:2,maxRetryDelayMs:100},responses:[{status:429,body:"wait",headers:{"retry-after-ms":"101"}}]},
 {name:"no midstream retry",options:{maxRetries:2},responses:[{body:frame(chunks[0])+'event: error\ndata: stream failure\n\n'}]},
 {name:"missing terminal message",responses:[{body:chunks.slice(0,-1).map(frame).join("")}]},
 {name:"fractional timeout rejected",options:{timeoutMs:1.5}},
 {name:"negative timeout rejected",options:{timeoutMs:-1}},
 {name:"explicit timeout header",options:{timeoutMs:2500}},
 {name:"timeout header deleted",options:{timeoutMs:2500,headers:{"X-Stainless-Timeout":null}}},
 {name:"timeout header overridden",options:{timeoutMs:2500,headers:{"X-Stainless-Timeout":"custom"}}},
 {name:"format conflict precedes timeout validation",options:{timeoutMs:-1},replacement:{output_format:{type:"json_schema"},output_config:{format:{type:"json_schema"}}}},
 {name:"payload failure precedes auth header validation",options:{headers:{"X-Api-Key":""}},fail:"payload"},
 {name:"timeout validation precedes auth header validation",options:{timeoutMs:-1,headers:{"X-Api-Key":""}}},
 {name:"workspace payload overrides headers",options:{headers:{"anthropic-workspace-id":"header"}},replacement:{workspace_id:"payload"}},
 {name:"empty betas remove configured beta",options:{headers:{"anthropic-beta":"configured"}},replacement:{betas:[]}},
];
const ignoredHeaders=["x-stainless-lang","x-stainless-package-version","x-stainless-os","x-stainless-arch","x-stainless-runtime","x-stainless-runtime-version"];
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model,headers:{...model.headers,...input.model?.headers}};
 const options={apiKey:"fixture-key",...input.options};
 const requests:any[]=[];const callbacks:any[]=[];
 const resultStream=stream(chosen,normalizeContext(input.context??{messages:[]}),{
  ...options,
  fetch:async(url:any,init:any)=>{
   const headers=Object.fromEntries(new Headers(init.headers));for(const key of ignoredHeaders)delete headers[key];
   requests.push({path:new URL(url).pathname,query:new URL(url).search.slice(1),headers,body:init.body?JSON.parse(init.body):null});
   const response=input.responses?.[Math.min(requests.length-1,input.responses.length-1)]??{body:success};
   return new Response(response.body,{status:response.status??200,headers:{"content-type":"text/event-stream","x-test":"response",...response.headers}});
  },
  onPayload:(payload:any)=>{callbacks.push({type:"payload",payload});if(input.fail==="payload")throw new Error("payload failed");return input.replacement},
  onResponse:(response:any)=>{callbacks.push({type:"response",...response});if(input.fail==="response")throw new Error("response failed")},
  onProviderStreamEvent:(chunk:any)=>{callbacks.push({type:"chunk",chunk:JSON.parse(JSON.stringify(chunk))});if(input.fail==="chunk")throw new Error("chunk failed");if(input.mutate&&chunk.delta?.type==="text_delta"){chunk.delta.text="changed"}},
 });
 const result=await resultStream.result();const events=[];for await(const event of resultStream)events.push(event);
 cases.push({input,expected:{requests,callbacks,events,result}});
}
const file=new URL("pi-anthropic-http.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,sdkVersion:JSON.parse(readFileSync(new URL("../../third_party/pi/node_modules/@anthropic-ai/sdk/package.json",import.meta.url),"utf8")).version,ignoredHeaders,model,success,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic HTTP oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic HTTP cases against Pi ${manifest.commit}`);
