import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest,stream} from "./completions-oracle.ts";
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
for(const key of ["OPENAI_ORG_ID","OPENAI_PROJECT_ID","PI_CACHE_RETENTION"])delete process.env[key];
const model={id:"test",api:"openai-completions",provider:"openai",baseUrl:"https://example.test/v1",reasoning:false,input:["text","image"],maxTokens:8192,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2},headers:{"User-Agent":"fixture"}};
const chunk=(content:string,finish_reason:any=null)=>({id:"response",choices:[{delta:{content},finish_reason}]});
const frame=(value:any)=>`data: ${JSON.stringify(value)}\n\n`;
const success=frame(chunk("hello"))+frame(chunk("!","stop"))+"data: [DONE]\n\n";
const inputs:any[]=[
 {name:"success"},
 {name:"missing api key",options:{apiKey:null}},
 {name:"model header alone does not supply api key",model:{headers:{"Authorization":"model-key"}},options:{apiKey:null}},
 {name:"authorization header supplies key",options:{apiKey:null,headers:{authorization:"header-key"}}},
 {name:"gateway header supplies key",options:{apiKey:"",headers:{"CF-AIG-Authorization":"gateway-key",Authorization:null}}},
 {name:"empty authorization rejected",options:{headers:{Authorization:""}}},
 {name:"header overrides and deletions",model:{headers:{"User-Agent":"fixture","X-One":"model","x-delete":"remove","x-case":"later"}},options:{sessionId:"session",headers:{"x-one":"option","x-delete":null,"content-type":"ignored","X-Case":"option","Authorization":null}}},
 {name:"exact spelling merge before case normalization",model:{headers:{"User-Agent":"old","user-agent":"later"}},options:{headers:{"User-Agent":"override"}}},
 {name:"openrouter session",model:{provider:"openrouter"},options:{sessionId:"session"}},
 {name:"cache none omits affinity",options:{cacheRetention:"none",sessionId:"session"}},
 {name:"copilot dynamic headers",model:{provider:"github-copilot"},context:{messages:[{role:"toolResult",toolCallId:"c",toolName:"tool",content:[{type:"image",mimeType:"image/png",data:"YQ=="}],isError:false,timestamp:1}]}},
 {name:"payload replacement",replacement:{model:"replaced",messages:[],stream:true}},
 {name:"payload callback fails",fail:"payload"},
 {name:"response callback fails",fail:"response"},
 {name:"chunk callback fails",fail:"chunk"},
 {name:"chunk callback mutates",mutate:true},
 {name:"http plain error",responses:[{status:403,body:"gateway denied"}]},
 {name:"http structured error",responses:[{status:400,body:JSON.stringify({error:{message:"bad request",type:"invalid",metadata:{raw:"opaque"}}})}]},
 {name:"http structured raw metadata newline",responses:[{status:400,body:JSON.stringify({error:{message:"bad",metadata:{raw:"line1\nline2"}}})}]},
 {name:"http empty error",responses:[{status:401,body:""}]},
 {name:"http unexpected json envelope",responses:[{status:400,body:JSON.stringify({message:"not in error field"})}]},
 {name:"http empty object",responses:[{status:400,body:"{}"}]},
 {name:"http array error",responses:[{status:400,body:'[{"message":"bad"}]'}]},
 {name:"http scalar error",responses:[{status:400,body:'"bad"'}]},
 {name:"http no content",responses:[{status:204,body:null}]},
 {name:"retry 429",options:{maxRetries:2},responses:[{status:429,body:JSON.stringify({error:{message:"wait"}}),headers:{"retry-after-ms":"0"}},{status:200,body:success}]},
 {name:"retry directive allows 400",options:{maxRetries:1},responses:[{status:400,body:"retry",headers:{"x-should-retry":"true","retry-after-ms":"0"}},{status:200,body:success}]},
 {name:"retry directive denies 500",options:{maxRetries:2},responses:[{status:500,body:"no retry",headers:{"x-should-retry":"false"}}]},
 {name:"retry delay cap",options:{maxRetries:2,maxRetryDelayMs:100},responses:[{status:429,body:"wait",headers:{"retry-after-ms":"101"}}]},
 {name:"no midstream retry",options:{maxRetries:2},responses:[{status:200,body:frame(chunk("partial"))+frame({error:{message:"stream failure"}})}]},
 {name:"named error event",responses:[{body:'event: error\ndata: {"message":"named failure"}\n\n'}]},
 {name:"thread event envelope",responses:[{body:'event: thread.created\ndata: {"content":"ignored"}\n\n'+success}]},
 {name:"invalid SSE JSON",responses:[{body:frame(chunk("partial"))+"data: {invalid\n\n"}]},
 {name:"unterminated final event",responses:[{body:frame(chunk("hello"))+"data: "+JSON.stringify(chunk("!","stop"))}]},
 {name:"CR and multiline SSE",responses:[{body:':comment\revent: completion\rdata: {"choices":\rdata: [{"delta":{"content":"ok"},"finish_reason":"stop"}]}\r\rdata: [DONE]\r\r'}]},
 {name:"BOM on each line",responses:[{body:'\ufeffdata: '+JSON.stringify(chunk("BOM","stop"))+'\n\n\ufeffdata: [DONE]\n\n'}]},
 {name:"sentinel ignores trailing malformed event",responses:[{body:success+"data: invalid\n\n"}]},
 {name:"empty data is malformed JSON",responses:[{body:"data:\n\n"}]},
 {name:"non-object chunks reach callback",responses:[{body:frame(null)+frame(7)+frame([])+success}]},
 {name:"fractional timeout rejected",options:{timeoutMs:1.5}},
 {name:"negative timeout rejected",options:{timeoutMs:-1}},
 {name:"explicit timeout header",options:{timeoutMs:2500}},
 {name:"explicit timeout header deleted",options:{timeoutMs:2500,headers:{"X-Stainless-Timeout":null}}},
 {name:"explicit timeout header overridden",options:{timeoutMs:2500,headers:{"X-Stainless-Timeout":"custom"}}},
 {name:"payload disables stream",replacement:{stream:false},responses:[{body:'{"choices":[]}'}]},
 {name:"payload disables stream JSON object",replacement:{stream:false},responses:[{body:'{"choices":[]}',headers:{"content-type":"application/json"}}]},
 {name:"payload disables stream JSON array",replacement:{stream:false},responses:[{body:JSON.stringify([chunk("array","stop")]),headers:{"content-type":"application/json"}}]},
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
   requests.push({path:new URL(url).pathname,headers,body:init.body?JSON.parse(init.body):null});
   const response=input.responses?.[Math.min(requests.length-1,input.responses.length-1)]??{body:success};
   return new Response(response.body,{status:response.status??200,headers:{"content-type":"text/event-stream","x-test":"response",...response.headers}});
  },
  onPayload:(payload:any)=>{callbacks.push({type:"payload",payload});if(input.fail==="payload")throw new Error("payload failed");return input.replacement},
  onResponse:(response:any)=>{callbacks.push({type:"response",...response});if(input.fail==="response")throw new Error("response failed")},
  onProviderStreamEvent:(chunk:any)=>{callbacks.push({type:"chunk",chunk:JSON.parse(JSON.stringify(chunk))});if(input.fail==="chunk")throw new Error("chunk failed");if(input.mutate&&chunk.choices?.[0]?.delta){chunk.choices[0].delta.content="changed"}},
 });
 const result=await resultStream.result();const events=[];for await(const event of resultStream)events.push(event);
 cases.push({input,expected:{requests,callbacks,events,result}});
}
const file=new URL("pi-completions-http.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,sdkVersion:JSON.parse(readFileSync(new URL("../../third_party/pi/node_modules/openai/package.json",import.meta.url),"utf8")).version,ignoredHeaders,model,success,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Completions HTTP oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} completions HTTP cases against Pi ${manifest.commit}`);
