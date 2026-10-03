import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {streamSimple}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
for(const name of ["PI_CACHE_RETENTION","ANTHROPIC_CUSTOM_HEADERS","ANTHROPIC_FEDERATION_RULE_ID","ANTHROPIC_ORGANIZATION_ID","ANTHROPIC_IDENTITY_TOKEN_FILE"])delete process.env[name];
// Reuse the established option/estimate inputs, but execute the original
// Anthropic streamSimple and provider to derive every expected value anew.
const seed=JSON.parse(readFileSync(new URL("pi-simple-options.json",import.meta.url),"utf8"));
const model={...seed.model,api:"anthropic-messages",provider:"anthropic",baseUrl:"https://api.anthropic.com"};
const inputs:any[]=seed.cases.map((entry:any)=>entry.input);
for(const input of inputs)for(const message of input.context?.messages??[])if(message.api==="openai-completions")message.api="anthropic-messages";
for(const level of [undefined,"off","minimal","low","medium","high","xhigh","max","unknown"]){
 inputs.push({name:`adaptive ${level}`,model:{compat:{forceAdaptiveThinking:true}},options:{reasoning:level}});
 inputs.push({name:`adaptive mapped ${level}`,model:{compat:{forceAdaptiveThinking:true},thinkingLevelMap:{off:"low",minimal:"low",low:"medium",medium:"high",high:"max",xhigh:"xhigh",max:"max",unknown:"custom"}},options:{reasoning:level}});
}
inputs.push(
 {name:"missing credentials",options:{apiKey:null}},
 {name:"header credentials",options:{apiKey:null,headers:{Authorization:"header-key"}}},
 {name:"x-api-key credentials",options:{apiKey:null,headers:{"X-Api-Key":"header-key"}}},
 {name:"model header is not credential source",model:{headers:{Authorization:"model-key"}},options:{apiKey:null}},
 {name:"provider options discarded",options:{effort:"max",thinkingEnabled:true,thinkingBudgetTokens:777,thinkingDisplay:"omitted",interleavedThinking:false}},
 {name:"custom thinking budget",model:{contextWindow:200000,maxTokens:64000},options:{reasoning:"medium",maxTokens:1000,thinkingBudgets:{medium:2222}}},
 {name:"zero custom thinking budget",options:{reasoning:"low",thinkingBudgets:{low:0}}},
 {name:"null custom thinking budget",options:{reasoning:"low",thinkingBudgets:{low:null}}},
 {name:"adaptive null mapping defaults",model:{compat:{forceAdaptiveThinking:true},thinkingLevelMap:{low:null}},options:{reasoning:"low"}},
 {name:"managed effort",model:{compat:{supportsMidConvoEffort:true,forceAdaptiveThinking:true}},options:{reasoning:"medium"}},
 {name:"OAuth simple request",options:{apiKey:"sk-ant-oat-fixture",reasoning:"low"}},
 {name:"no reasoning model still budgets",model:{reasoning:false,contextWindow:200000},options:{reasoning:"high",maxTokens:1000}},
);
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model};const options={apiKey:"fixture-key",...input.options};
 const transcript=normalizeContext(input.context??{messages:[]});
 let params:any=null,result:any=null,error:string|null=null;
 try {
 const stream=streamSimple(chosen,transcript,{...options,onPayload:(payload:any)=>{params=payload},fetch:async()=>new Response('event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}\n\n',{headers:{"content-type":"text/event-stream"}})});
 result=await stream.result();for await(const _ of stream){}
 }catch(failure){error=(failure as Error).message}
 cases.push({input,expected:{params,result,error}});
}
const file=new URL("pi-anthropic-simple.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic simple oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic simple cases against Pi ${manifest.commit}`);
