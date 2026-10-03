import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest,oracleParams} from "./completions-oracle.ts";
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
delete process.env.PI_CACHE_RETENTION;
const model={id:"test",name:"Test",api:"openai-completions",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text","image"],maxTokens:20000,contextWindow:100000,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
const user=(content:any)=>({role:"user",content,timestamp:100});
const text=(text:string)=>({type:"text",text});
const image={type:"image",mimeType:"image/png",data:"YQ=="};
const tool={name:"tool",description:"Tool",parameters:{type:"object",properties:{code:{type:"string"}},required:["code"]}};
const context={systemPrompt:"system",messages:[user("hello")],tools:[tool]};
const assistant=(content:any[],extra:any={})=>({role:"assistant",content,stopReason:"stop",timestamp:100,api:model.api,provider:model.provider,model:model.id,...extra});
const formats=["openai","zai","qwen","qwen-chat-template","chat-template","baseten","deepseek","openrouter","ant-ling","together","string-thinking"];
const template={enabled:{$var:"thinking.enabled"},effort:{$var:"thinking.effort"},budget:{$var:"thinking.budget"},optional:{$var:"thinking.enabled",omitWhenOff:true},literal:null,preserve:true,count:3,label:"text"};
const inputs:any[]=[
 {name:"default"},
 {name:"named options",options:{maxTokens:4096,temperature:0,toolChoice:{type:"function",function:{name:"tool"}},sessionId:"session"}},
 {name:"zero max tokens",options:{maxTokens:0}},
 {name:"no reasoning",model:{reasoning:false},options:{reasoningEffort:"high"}},
 {name:"disabled usage and store",compat:{supportsUsageInStreaming:false,supportsStore:false,zaiToolStream:true,vllmPriority:0}},
 {name:"max_tokens dialect",compat:{maxTokensField:"max_tokens"},options:{maxTokens:4096,reasoningEffort:"high"}},
 {name:"sampling override order",model:{samplingParams:{temperature:0.5,model:"model-default",nested:{model:true}}},options:{temperature:0.9,samplingParams:{model:"request-override",stream:false,temperature:null,nested:{request:true}}}},
 {name:"no tools",context:{messages:[user("hello")]}},
 {name:"tool history sends empty declarations",context:{messages:[assistant([{type:"toolCall",id:"c",name:"tool",arguments:{}}]),{role:"toolResult",toolCallId:"c",toolName:"tool",content:[text("done")],timestamp:100,isError:false}]}},
 {name:"routing",compat:{openRouterRouting:{only:["one"]},vercelGatewayRouting:{only:[],order:["two"],ignored:"x"}}},
 {name:"empty routing",compat:{openRouterRouting:{},vercelGatewayRouting:{}}},
 {name:"null priority",compat:{vllmPriority:null}},
 ...formats.flatMap(thinkingFormat=>[undefined,"high"].flatMap(reasoningEffort=>[undefined,{high:"mapped",off:"disabled"},{high:null,off:null}].map((thinkingLevelMap,i)=>({name:`thinking ${thinkingFormat} ${reasoningEffort??"off"} ${i}`,model:{thinkingLevelMap},compat:{thinkingFormat,supportsReasoningEffort:true,chatTemplateKwargs:template,chatTemplateArgs:template,supportsThinkingTokenBudget:true},options:{reasoningEffort}})))),
 ...["minimal","low","medium","high","xhigh","max"].map(reasoningEffort=>({name:`budget ${reasoningEffort}`,compat:{supportsThinkingTokenBudget:true},options:{reasoningEffort,maxTokens:8000}})),
 ...[0,512,1024,1025,4096].map(maxTokens=>({name:`budget ceiling ${maxTokens}`,compat:{thinkingTokenBudgetField:"thinking_budget_tokens"},options:{reasoningEffort:"high",maxTokens}})),
 {name:"custom budget",compat:{thinkingTokenBudgetField:"thinking_budget",supportsThinkingTokenBudget:true},options:{reasoningEffort:"max",thinkingBudgets:{high:1234,max:99999},maxTokens:5000}},
 {name:"budget independent of format",compat:{thinkingFormat:"qwen-chat-template",supportsThinkingTokenBudget:true},options:{reasoningEffort:"low",maxTokens:3000}},
 ...["none","short","long"].flatMap(cacheRetention=>["openai","openrouter","together"].map(provider=>({name:`cache ${provider} ${cacheRetention}`,model:{id:provider==="openrouter"?"anthropic/claude":"test",provider,baseUrl:provider==="openai"?model.baseUrl:`https://${provider}.example/v1`},options:{cacheRetention,sessionId:"😀".repeat(70)}}))),
 {name:"scoped cache env",options:{env:{PI_CACHE_RETENTION:"long"},sessionId:"id"}},
 {name:"process cache env",processCache:"long",options:{sessionId:"id"}},
 {name:"empty scoped cache uses process",processCache:"long",options:{env:{PI_CACHE_RETENTION:""},sessionId:"id"}},
 {name:"explicit cache overrides env",processCache:"long",options:{cacheRetention:"none",env:{PI_CACHE_RETENTION:"long"},sessionId:"id"}},
 {name:"cache skips empty tail and images",compat:{cacheControlFormat:"anthropic"},context:{systemPrompt:"system",messages:[user("anchor"),user([image]),assistant([{type:"toolCall",id:"c",name:"tool",arguments:{}}])],tools:[tool]}},
 {name:"cache last text part",compat:{cacheControlFormat:"anthropic"},context:{messages:[user([text("one"),text("two"),image])]}},
 {name:"cache tool text",compat:{cacheControlFormat:"anthropic"},context:{messages:[assistant([{type:"toolCall",id:"c",name:"tool",arguments:{}}]),{role:"toolResult",toolCallId:"c",toolName:"tool",content:[text("done")],timestamp:100,isError:false}]}},
 {name:"cache no messages",compat:{cacheControlFormat:"anthropic"},context:{messages:[]}},
 {name:"grammar declarations",compat:{supportsOpenAIGrammarTools:true,cacheControlFormat:"anthropic"},context:{messages:[user("run")],tools:[{...tool,constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}}]}},
 {name:"strict declaration failure",context:{messages:[],tools:[{...tool,constrainedSampling:{type:"json_schema",strict:"require"}}]}},
 {name:"mid conversation tool additions",compat:{supportsMidConvoSystemMessages:true,supportsMidConvoToolAdditions:true},context:{messages:[{role:"system",content:"system",timestamp:1},user("run"),{role:"system",content:"",timestamp:2,toolsAdded:[tool]},user("next")]}},
];
const cases=[];
for(const input of inputs){
 if(input.processCache)process.env.PI_CACHE_RETENTION=input.processCache;else delete process.env.PI_CACHE_RETENTION;
 const chosen={...model,...input.model,compat:input.compat};
 const transcript=normalizeContext(input.context??context);
 const before=JSON.stringify({chosen,transcript,input});
 let params:any=null,error:string|null=null;
 try{params=oracleParams(chosen,transcript,input.options)}catch(failure){error=(failure as Error).message}
 if(JSON.stringify({chosen,transcript,input})!==before)throw new Error("request builder mutated inputs");
 cases.push({input,expected:{params,error}});
}
const file=new URL("pi-completions-params.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,context,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Completions request oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} completions request-body cases against Pi ${manifest.commit}`);
