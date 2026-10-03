import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {streamSimple}=await import(new URL("api/openai-responses.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {buildBaseOptions}=await import(new URL("api/simple-options.ts",aiRoot).pathname);
const {clampThinkingLevel}=await import(new URL("models.ts",aiRoot).pathname);
Date.now=()=>100;
delete process.env.PI_CACHE_RETENTION;
// Reuse the established option/estimate inputs, but execute the original
// Responses streamSimple and provider to derive every expected value anew.
const seed=JSON.parse(readFileSync(new URL("pi-simple-options.json",import.meta.url),"utf8"));
const model={...seed.model,api:"openai-responses"};
const inputs:any[]=seed.cases.map((entry:any)=>entry.input);
for(const input of inputs)for(const message of input.context?.messages??[])if(message.api==="openai-completions")message.api="openai-responses";
inputs.push(
 {name:"provider-only options discarded",options:{reasoningEffort:"high",reasoningSummary:"detailed",serviceTier:"priority",thinkingBudgets:{high:500},reasoning:"low"}},
 {name:"sampling sets provider-only fields",options:{samplingParams:{service_tier:"priority",reasoning:{effort:"high",summary:"detailed"}}}},
 {name:"missing credentials",options:{apiKey:null}},
 {name:"null header credential option",options:{apiKey:null,headers:{Authorization:"header-key"}}},
 {name:"header credentials",options:{apiKey:"",headers:{Authorization:"header-key"}}},
 {name:"model header is not credential source",model:{headers:{Authorization:"model-key"}},options:{apiKey:null}},
 {name:"gateway credentials",options:{apiKey:"",headers:{"CF-AIG-Authorization":"gateway-key"}}},
 {name:"chatgpt credential",options:{apiKey:"oauth",maxTokens:500,temperature:0.5,cacheRetention:"long"}},
 {name:"strict schema fails after admission",context:{messages:[],tools:[{name:"tool",description:"tool",parameters:{type:"object",properties:{}},constrainedSampling:{type:"json_schema",strict:"require"}}]}},
 {name:"token floor after exhausted context",model:{contextWindow:4096},options:{maxTokens:8192}},
 {name:"reasoning off blocked",model:{thinkingLevelMap:{off:null}},options:{reasoning:"off"}},
);
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model};const options={apiKey:"fixture-key",...input.options};
 const transcript=normalizeContext(input.context??{messages:[]});
 const level=options.reasoning?clampThinkingLevel(chosen,options.reasoning):undefined;
 const prepared={...buildBaseOptions(chosen,transcript,options,options.apiKey),toolChoice:options.toolChoice,reasoningEffort:level==="off"?undefined:level};
 let params:any=null,result:any=null,error:string|null=null;
 try {
 const stream=streamSimple(chosen,transcript,{...options,onPayload:(payload:any)=>{params=payload},fetch:async()=>new Response('data: {"type":"response.completed","response":{"id":"response","status":"completed"}}\n\ndata: [DONE]\n\n',{headers:{"content-type":"text/event-stream"}})});
 result=await stream.result();for await(const _ of stream){}
 }catch(failure){error=(failure as Error).message}
 cases.push({input,expected:{options:prepared,params,result,error}});
}
const file=new URL("pi-responses-simple.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Responses simple oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Responses simple cases against Pi ${manifest.commit}`);
