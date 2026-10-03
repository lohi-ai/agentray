import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest,streamSimple} from "./completions-oracle.ts";
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {estimateContextTokens,estimateMessageTokens}=await import(new URL("utils/estimate.ts",aiRoot).pathname);
const {buildBaseOptions,adjustMaxTokensForThinking}=await import(new URL("api/simple-options.ts",aiRoot).pathname);
const {getSupportedThinkingLevels,clampThinkingLevel}=await import(new URL("models.ts",aiRoot).pathname);
Date.now=()=>100;
delete process.env.PI_CACHE_RETENTION;
const model={id:"test",name:"Test",api:"openai-completions",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text","image"],contextWindow:4200,maxTokens:8192,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
const user=(content:any,timestamp=1)=>({role:"user",content,timestamp});
const text=(text:string)=>({type:"text",text});
const tool={name:"tool",description:"<>&😀\u2028",parameters:{type:"object",properties:{input:{type:"string"}},required:["input"]}};
const assistant=(content:any[],usage:any={},extra:any={})=>({role:"assistant",content,api:model.api,provider:model.provider,model:model.id,timestamp:2,stopReason:"stop",usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:model.cost,...usage},...extra});
const levels=["off","minimal","low","medium","high","xhigh","max","unknown"];
const maps=[undefined,{high:null},{off:null},{minimal:null,low:null,medium:null,high:null},{high:null,xhigh:"extra"},{max:"maximum",xhigh:"extra"},{off:null,minimal:null,low:null,medium:null,high:null,xhigh:null,max:null}];
const inputs:any[]=[
 ...maps.flatMap((thinkingLevelMap,i)=>levels.map(reasoning=>({name:`level ${reasoning} map ${i}`,model:{thinkingLevelMap},options:{reasoning}}))),
 {name:"no reasoning model",model:{reasoning:false},options:{reasoning:"high"}},
 {name:"no reasoning requested"},
 {name:"option projection",options:{temperature:0,samplingParams:{temperature:0.2},maxTokens:50,transport:"sse",cacheRetention:"long",sessionId:"session",headers:{"x-extra":"header"},timeoutMs:1000,websocketConnectTimeoutMs:300,maxRetries:2,maxRetryDelayMs:100,metadata:{a:1},env:{PI_CACHE_RETENTION:"short"},toolChoice:"none",thinkingBudgets:{high:50},reasoning:"high",reasoningEffort:"discarded",unused:"discarded"}},
 ...[0,-5,1,100,8192,null].flatMap(maxTokens=>[0,4096,4200].map(contextWindow=>({name:`max ${maxTokens} window ${contextWindow}`,model:{contextWindow},options:{maxTokens}}))),
 {name:"text UTF16 estimate",context:{messages:[user("😀".repeat(7)+"x")]}},
 {name:"images estimate",context:{messages:[user([text("hello"),{type:"image",data:"YQ==",mimeType:"image/png"}])]}},
 {name:"system sections tools",context:{messages:[{role:"system",content:"ignored",sections:{first:"text",second:"😀"},toolsAdded:[tool],toolsRemoved:[{name:"old"}],timestamp:1},user("hello")]}},
 {name:"assistant text thinking args",context:{messages:[user("hello"),assistant([text("answer"),{type:"thinking",thinking:"😀think"},{type:"toolCall",id:"c",name:"tool",arguments:{"2":"second","1":"first",code:"<>&\u2028"}}])]}},
 {name:"tool result estimate",context:{messages:[{role:"toolResult",toolCallId:"c",toolName:"tool",content:[text("done"),{type:"image",data:"YQ==",mimeType:"image/png"}],isError:false,timestamp:3}]}},
 {name:"usage total takes precedence",context:{messages:[user("hello"),assistant([text("answer")],{input:10,output:5,cacheRead:2,cacheWrite:3,totalTokens:40}),user("next",3)]}},
 {name:"usage fallback sum",context:{messages:[user("hello"),assistant([text("answer")],{input:10,output:5,cacheRead:2,cacheWrite:3}),user("next",3)]}},
 {name:"stale usage after inserted prefix",context:{messages:[user("new summary",100),assistant([text("old answer")],{totalTokens:900}),user("new turn",101)]}},
 {name:"equal timestamp usage accepted",context:{messages:[user("summary",2),assistant([text("answer")],{totalTokens:10})]}},
 {name:"error usage ignored",context:{messages:[assistant([text("error")],{totalTokens:900},{stopReason:"error"})]}},
 {name:"aborted usage ignored",context:{messages:[assistant([text("aborted")],{totalTokens:900},{stopReason:"aborted"})]}},
 {name:"earlier usage retained after failed response",context:{messages:[assistant([text("first")],{totalTokens:10}),user("next",3),assistant([text("error")],{totalTokens:900},{timestamp:4,stopReason:"error"})]}},
 {name:"latest usage wins",context:{messages:[assistant([text("first")],{totalTokens:10}),user("next",3),assistant([text("second")],{totalTokens:70},{timestamp:4}),user("tail",5)]}},
 {name:"tool declarations without messages",context:{messages:[],tools:[tool]}},
 {name:"context budget caps thinking",model:{contextWindow:16000,compat:{supportsThinkingTokenBudget:true}},options:{reasoning:"high"},context:{messages:[assistant([text("answer")],{totalTokens:10000})]}},
];
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model};const options={apiKey:"fixture-key",...input.options};
 const transcript=normalizeContext(input.context??{messages:[]});
 const level=options.reasoning?clampThinkingLevel(chosen,options.reasoning):undefined;
 const prepared={...buildBaseOptions(chosen,transcript,options,options.apiKey),toolChoice:options.toolChoice,reasoningEffort:level==="off"?undefined:level,thinkingBudgets:options.thinkingBudgets};
 let params:any=null;
 const stream=streamSimple(chosen,transcript,{...options,onPayload:(payload:any)=>{params=payload},fetch:async()=>new Response('data: {"choices":[{"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n',{headers:{"content-type":"text/event-stream"}})});
 await stream.result();
 cases.push({input,expected:{options:prepared,params,estimate:estimateContextTokens(transcript),messageTokens:transcript.messages.map(estimateMessageTokens),levels:getSupportedThinkingLevels(chosen)}});
}
const budgets=[];
for(const level of ["minimal","low","medium","high","xhigh","max"]){
 for(const base of [undefined,0,1000,20000])for(const ceiling of [512,8192,32768]){
  const custom={high:12345};budgets.push({input:{level,base,ceiling,custom},expected:adjustMaxTokensForThinking(base,ceiling,level,custom)});
 }
}
const file=new URL("pi-simple-options.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,cases,budgets},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Simple options oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} simple/estimate cases and ${budgets.length} thinking-budget cases against Pi ${manifest.commit}`);
