import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"responses-params-oracle",setup(build){build.onLoad({filter:/\/api\/openai-responses\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {getCompat as oracleCompat,buildParams as oracleParams};\n"}))}});
const {oracleCompat,oracleParams}=await import(new URL("api/openai-responses.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",name:"Test",api:"openai-responses",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text","image"],maxTokens:20000,contextWindow:100000,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
const user=(content:any)=>({role:"user",content,timestamp:100});
const tool={name:"tool",description:"Tool",parameters:{type:"object",properties:{code:{type:"string"}},required:["code"]}};
const context={systemPrompt:"system",messages:[user("hello")],tools:[tool]};
const inputs:any[]=[
 {name:"default"},
 {name:"named options",options:{maxTokens:4096,temperature:0,serviceTier:"priority",toolChoice:{type:"function",name:"tool"},sessionId:"session",reasoningEffort:"high"}},
 ...[0,-1,1,15,16,17,null].map(maxTokens=>({name:`token floor ${maxTokens}`,options:{maxTokens}})),
 {name:"max tokens unsupported",compat:{supportsMaxOutputTokens:false},options:{maxTokens:500}},
 ...[false,true].flatMap(reasoning=>["openai","github-copilot","xai"].flatMap(provider=>[{}, {off:null,high:null},{off:"disabled",high:"mapped"}].flatMap((thinkingLevelMap,i)=>[{}, {reasoningEffort:"high"},{reasoningSummary:"concise"},{reasoningSummary:null}].map((options,j)=>({name:`reasoning ${reasoning} ${provider} map ${i} options ${j}`,model:{reasoning,provider,thinkingLevelMap},options}))))),
 ...["none","short","long"].flatMap(cacheRetention=>[{}, {supportsExplicitPromptCacheMode:true},{supportsLongCacheRetention:false},{supportsLongCacheRetention:false,supportsExplicitPromptCacheMode:true}].map((compat,i)=>({name:`cache ${cacheRetention} mode ${i}`,compat,options:{cacheRetention,sessionId:"😀".repeat(70)}}))),
 ...[undefined,"", "sk-test","oauth-token"].map(apiKey=>({name:`chatgpt key ${apiKey??"absent"}`,options:{apiKey,maxTokens:500,temperature:0.5,cacheRetention:"long",sessionId:"session",serviceTier:"flex"}})),
 {name:"chatgpt explicit cache",compat:{supportsExplicitPromptCacheMode:true},options:{apiKey:"oauth",cacheRetention:"none",maxTokens:400}},
 {name:"chatgpt other url",model:{baseUrl:"https://api.openai.com/v1/"},options:{apiKey:"oauth",maxTokens:500,temperature:0.2,cacheRetention:"long"}},
 {name:"chatgpt other provider",model:{provider:"custom"},options:{apiKey:"oauth",maxTokens:500,temperature:0.2,cacheRetention:"long"}},
 {name:"nullable options",options:{temperature:null,serviceTier:null,toolChoice:null}},
 {name:"sampling overrides",model:{samplingParams:{model:"default",stream:false,temperature:0.1,nested:{model:true}}},options:{temperature:0.8,samplingParams:{model:"override",temperature:null,nested:{request:true},store:true}}},
 {name:"scoped env",options:{env:{PI_CACHE_RETENTION:"long"},sessionId:"id"}},
 {name:"process env",processCache:"long",options:{sessionId:"id"}},
 {name:"empty scoped falls back",processCache:"long",options:{env:{PI_CACHE_RETENTION:""},sessionId:"id"}},
 {name:"explicit none beats env",processCache:"long",options:{env:{PI_CACHE_RETENTION:"long"},cacheRetention:"none",sessionId:"id"}},
 {name:"empty session",options:{sessionId:""}},
 {name:"router provider",model:{provider:"openrouter"}},
 {name:"router url",model:{baseUrl:"https://openrouter.ai/api/v1"}},
 {name:"null overrides",compat:{supportsDeveloperRole:null,supportsStrictMode:null,supportsMaxOutputTokens:null,sessionAffinityFormat:null}},
 {name:"explicit overrides",compat:{supportsDeveloperRole:false,supportsStrictMode:true,supportsLongCacheRetention:false,sessionAffinityFormat:"custom"}},
 {name:"no tools",context:{messages:[user("hi")]}},
 {name:"strict required unsupported",context:{messages:[],tools:[{...tool,constrainedSampling:{type:"json_schema",strict:"require"}}]}},
 {name:"strict required supported",compat:{supportsStrictMode:true},context:{messages:[],tools:[{...tool,constrainedSampling:{type:"json_schema",strict:"require"}}]}},
 {name:"grammar tools",compat:{supportsOpenAIGrammarTools:true},context:{messages:[user("run")],tools:[{...tool,constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}}]}},
 ...[{}, {supportsMidConvoSystemMessages:true,supportsAdditionalTools:true},{supportsMidConvoSystemMessages:true,supportsToolSearch:true}].map((compat,i)=>({name:`mid conversation tools ${i}`,compat,context:{messages:[{role:"system",content:"system",timestamp:1},user("run"),{role:"system",content:"",timestamp:2,toolsAdded:[tool]},user("next")]}})),
];
const cases=[];
for(const input of inputs){
 if(input.processCache)process.env.PI_CACHE_RETENTION=input.processCache;else delete process.env.PI_CACHE_RETENTION;
 const chosen={...model,...input.model,compat:input.compat};const transcript=normalizeContext(input.context??context);
 const before=JSON.stringify({chosen,transcript,input});const compat=oracleCompat(chosen);
 let params:any=null,error:string|null=null;
 try{params=oracleParams(chosen,transcript,input.options)}catch(failure){error=(failure as Error).message}
 if(JSON.stringify({chosen,transcript,input})!==before)throw new Error("request builder mutated inputs");
 cases.push({input,expected:{compat,params,error}});
}
const file=new URL("pi-responses-params.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,context,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Responses request oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Responses request-body cases against Pi ${manifest.commit}`);
