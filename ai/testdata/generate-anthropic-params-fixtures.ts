import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"anthropic-params-oracle",setup(build){build.onLoad({filter:/\/api\/anthropic-messages\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {getAnthropicCompat as oracleCompat,buildParams as oracleParams};\n"}))}});
const {oracleCompat,oracleParams}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext,resolveTranscript}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"claude-test",name:"Test",api:"anthropic-messages",provider:"anthropic",baseUrl:"https://api.anthropic.com",reasoning:true,input:["text","image"],maxTokens:8192,contextWindow:200000,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2}};
const text=(text:string)=>({type:"text",text});
const image={type:"image",mimeType:"image/png",data:"YQ=="};
const user=(content:any)=>({role:"user",content,timestamp:100});
const assistant=(content:any[],extra:any={})=>({role:"assistant",content,stopReason:"stop",timestamp:100,api:model.api,provider:model.provider,model:model.id,...extra});
const thinking=(thinking:string,thinkingSignature?:string,extra:any={})=>({type:"thinking",thinking,thinkingSignature,...extra});
const call=(id="call",name="read",args:any={file:"<>&\u2028😀"})=>({type:"toolCall",id,name,arguments:args});
const result=(id="call",content:any[]=[text("result")])=>({role:"toolResult",toolCallId:id,toolName:"read",content,isError:false,timestamp:100});
const tool={name:"read",description:"Read",parameters:{type:"object",properties:{file:{type:"string"}},required:["file"],additionalProperties:false,description:"root annotation"}};
const changed={...tool,description:"New read",parameters:{type:"object",properties:{path:{type:"string"}}}};
const system=(content="",extra:any={})=>({role:"system",content,timestamp:100,...extra});
const context={systemPrompt:"system",messages:[user("hello")],tools:[tool]};
const updates=[system("first",{toolsAdded:[tool]}),user("hi"),assistant([call()]),system("changed",{toolsAdded:[changed,{...tool,name:"write"}],toolsRemoved:[{name:"read"},{name:"old"}]}),result(),user("next"),assistant([text("answer")],{providerThinkingLevel:"low"}),system("last",{toolsRemoved:[{name:"write"}]})];
const inputs:any[]=[
 {name:"default"},
 {name:"empty context",context:{messages:[]}},
 {name:"oauth identity",oauth:true},
 {name:"oauth no prompt",oauth:true,context:{messages:[]}},
 {name:"oauth Unicode name casing",oauth:true,context:{messages:[assistant([call("a","baſh"),call("b","Skİll"),call("c","KillShell")])],tools:[{...tool,name:"baſh"},{...tool,name:"Skİll"},{...tool,name:"KillShell"}]}},
 {name:"system sections",context:{messages:[system("ignored",{sections:{first:"one",second:"two"}}),user("hi")]}},
 {name:"blank user filtered",context:{messages:[user(" \ufeff\n"),user([text(""),text(" \n"),image,text("yes")]),user([])]}},
 {name:"nonvision images",model:{input:["text"]},context:{messages:[user([image,image]),result("image",[image])]}},
 {name:"image tool result placeholder",context:{messages:[result("a",[image]),result("b",[text(""),image]),result("c",[text("one"),text("two")]),result("d",[])]}},
 {name:"tool result errors",context:{messages:[{...result(),isError:true}]}},
 {name:"assistant empty blocks",context:{messages:[assistant([text(" \n"),thinking(" ")]),assistant([text("answer")]),assistant([])]}},
 ...[undefined,"","  ","signature"].flatMap(signature=>[false,true].map(allowEmptySignature=>({name:`thinking signature ${signature??"undefined"} empty allowed ${allowEmptySignature}`,compat:{allowEmptySignature},context:{messages:[assistant([thinking("reason",signature),text("answer")])]}}))),
 {name:"signed empty thinking",context:{messages:[assistant([thinking("","opaque")])]}},
 {name:"redacted reasoning",context:{messages:[assistant([thinking("","opaque-redacted",{redacted:true}),text("answer")])]}},
 {name:"cross model thinking signatures",context:{messages:[assistant([thinking("reason","signature"),thinking("","redacted",{redacted:true}),text("answer")],{model:"other"})]}},
 {name:"tool null input default",context:{messages:[assistant([call("call","read",null)]),result()]}},
 {name:"same model id retained",context:{messages:[assistant([call("😀+/|"+"x".repeat(70))]),result("😀+/|"+"x".repeat(70))]}},
 {name:"foreign id normalized",context:{messages:[assistant([call("😀+/|"+"x".repeat(70))],{api:"openai-responses",provider:"openai"}),result("😀+/|"+"x".repeat(70))]}},
 {name:"oauth canonical tool names",oauth:true,context:{messages:[assistant([call("a","READ"),call("b","bash"),call("c","CustomTool")]),result("a"),result("b"),result("c")],tools:[tool,{...tool,name:"bAsH"},{...tool,name:"CustomTool"}]}},
 {name:"missing result repaired",context:{messages:[assistant([call()]),user("next")]}},
 {name:"rejected assistant calls removed",context:{messages:[assistant([call()],{stopReason:"error"}),assistant([text("aborted")],{stopReason:"aborted"}),user("next")]}},
 {name:"consecutive results grouped",context:{messages:[assistant([call("a"),call("b")]),result("a"),result("b"),user("next")]}},
 ...[{}, {supportsMidConvoSystemMessages:true},{supportsMidConvoSystemMessages:true,supportsMidConvoToolChanges:true},{supportsMidConvoSystemMessages:true,supportsMidConvoToolChanges:true,supportsMidConvoEffort:true}].flatMap((compat,i)=>[false,true].map(oauth=>({name:`system and tool updates ${i} oauth ${oauth}`,compat,oauth,context:{messages:updates}}))),
 {name:"pending system before empty assistant",compat:{supportsMidConvoSystemMessages:true},context:{messages:[user("first"),system("update"),user("second"),assistant([])]}},
 {name:"native tool changes need initial tool",compat:{supportsMidConvoSystemMessages:true,supportsMidConvoToolChanges:true},context:{messages:[system("first"),user("hi"),system("new tool",{toolsAdded:[tool]})]}},
 {name:"native tool addition cache marker",compat:{supportsMidConvoSystemMessages:true,supportsMidConvoToolChanges:true},context:{messages:[system("",{toolsAdded:[tool]}),user("hi"),system("",{toolsAdded:[changed]})]}},
 {name:"native tool removal cache marker",compat:{supportsMidConvoSystemMessages:true,supportsMidConvoToolChanges:true},context:{messages:[system("",{toolsAdded:[tool]}),user("hi"),system("",{toolsRemoved:[{name:"read"}]})]}},
 {name:"system does not break tool results",compat:{supportsMidConvoSystemMessages:true},context:{messages:[user("hi"),assistant([call("a"),call("b")]),result("a"),system("update"),result("b"),user("next"),assistant([text("answer")])]}},
 {name:"empty system update ignored",compat:{supportsMidConvoSystemMessages:true},context:{messages:[user("hi"),system(),assistant([text("answer")])]}},
 ...["low","medium","high","xhigh","max","off","invalid"].map(providerThinkingLevel=>({name:`managed effort history ${providerThinkingLevel}`,compat:{supportsMidConvoEffort:true},options:{effort:"low"},context:{messages:[assistant([text("answer")],{providerThinkingLevel})]}})),
 {name:"managed effort foreign API ignored",compat:{supportsMidConvoEffort:true},context:{messages:[assistant([text("answer")],{providerThinkingLevel:"low",api:"openai-completions"})]}},
 {name:"managed effort foreign provider ignored",compat:{supportsMidConvoEffort:true},context:{messages:[assistant([text("answer")],{providerThinkingLevel:"low",provider:"other"})]}},
 {name:"managed effort retained across model",compat:{supportsMidConvoEffort:true},context:{messages:[assistant([thinking("reason","sig"),text("answer")],{providerThinkingLevel:"low",model:"other"})]}},
 ...["none","short","long"].flatMap(cacheRetention=>[true,false].map(supportsLongCacheRetention=>({name:`cache ${cacheRetention} long supported ${supportsLongCacheRetention}`,compat:{supportsLongCacheRetention},options:{cacheRetention}}))),
 {name:"no tool cache",compat:{supportsCacheControlOnTools:false}},
 {name:"assistant tail no conversation cache",context:{messages:[user("hi"),assistant([text("answer")])]}},
 {name:"image tail cache",context:{messages:[user([image])]}},
 {name:"result tail cache",context:{messages:[result()]}},
 {name:"scoped cache env",options:{env:{PI_CACHE_RETENTION:"long"}}},
 {name:"process cache env",processCache:"long"},
 {name:"empty scoped env fallback",processCache:"long",options:{env:{PI_CACHE_RETENTION:""}}},
 {name:"explicit cache overrides env",processCache:"long",options:{cacheRetention:"none"}},
 ...[{}, {thinkingEnabled:true},{thinkingEnabled:false},{thinkingEnabled:true,interleavedThinking:false},{thinkingEnabled:true,thinkingBudgetTokens:0},{thinkingEnabled:true,thinkingBudgetTokens:4096,thinkingDisplay:"omitted"}].flatMap((options,i)=>[{}, {forceAdaptiveThinking:true},{supportsMidConvoEffort:true}].map((compat,j)=>({name:`thinking mode ${i} compat ${j}`,compat,options:{temperature:0,...options,effort:"medium"}}))),
 {name:"thinking off not supported",model:{thinkingLevelMap:{off:null}},options:{thinkingEnabled:false}},
 {name:"nonreasoning model",model:{reasoning:false},options:{thinkingEnabled:true,temperature:0.4}},
 {name:"temperature unsupported",compat:{supportsTemperature:false},options:{temperature:0.2}},
 ...[0,null,-1,1024].map(maxTokens=>({name:`max tokens ${maxTokens}`,options:{maxTokens}})),
 ...["auto","any","none",{type:"tool",name:"read"},{type:"auto",disable_parallel_tool_use:true},null].map((toolChoice,i)=>({name:`tool choice ${i}`,options:{toolChoice}})),
 {name:"metadata user only",options:{metadata:{user_id:"user",extra:"discard"}}},
 {name:"metadata empty user",options:{metadata:{user_id:""}}},
 {name:"metadata nonstring ignored",options:{metadata:{user_id:42}}},
 {name:"samplingParams not merged",model:{samplingParams:{model:"wrong"}},options:{samplingParams:{model:"wrong",stream:false}}},
 {name:"fallback models",compat:{allowedFallbackModels:[{model:"fallback",extra:"discard"}]}},
 {name:"fine grain beta",compat:{supportsEagerToolInputStreaming:false}},
 {name:"no tools no fine grain beta",compat:{supportsEagerToolInputStreaming:false},context:{messages:[]}},
 {name:"beta override case ordering",model:{headers:{"anthropic-beta":"model,one","Anthropic-Beta":"model,two"}},options:{headers:{"ANTHROPIC-BETA":" custom, ,custom,other "},thinkingEnabled:true}},
 {name:"beta removed",oauth:true,options:{headers:{"anthropic-beta":null},thinkingEnabled:true}},
 {name:"beta empty",options:{headers:{"anthropic-beta":""}}},
 {name:"router provider",model:{provider:"openrouter"}},
 {name:"router URL",model:{provider:"custom",baseUrl:"https://openrouter.ai/anthropic"}},
 {name:"compat nullish",compat:{supportsEagerToolInputStreaming:null,supportsTemperature:null,sessionAffinityFormat:null}},
 {name:"compat empty affinity",compat:{sessionAffinityFormat:""}},
 {name:"legacy schema defaults",context:{messages:[],tools:[{...tool,parameters:{type:"object"}}]}},
 ...[false,true].flatMap(supportsStrictTools=>["prefer","require"].map(strict=>({name:`strict ${strict} supported ${supportsStrictTools}`,compat:{supportsStrictTools},context:{messages:[],tools:[{...tool,constrainedSampling:{type:"json_schema",strict}}]}}))),
 ...["minimum","maximum","exclusiveMinimum","exclusiveMaximum","multipleOf","maxItems","uniqueItems","minContains","maxContains","minProperties","maxProperties"].flatMap(key=>["prefer","require"].map(strict=>({name:`strict keyword ${key} ${strict}`,compat:{supportsStrictTools:true},context:{messages:[],tools:[{...tool,parameters:{type:"object",properties:{value:{type:"number",[key]:1}}},constrainedSampling:{type:"json_schema",strict}}]}}))),
 ...[0,1,2,null,"1"].map(minItems=>({name:`strict minItems ${minItems} type ${typeof minItems}`,compat:{supportsStrictTools:true},context:{messages:[],tools:[{...tool,parameters:{type:"object",properties:{value:{type:"array",items:{type:"string"},minItems}}},constrainedSampling:{type:"json_schema",strict:"require"}}]}})),
 ...["date-time","time","date","duration","email","hostname","uri","ipv4","ipv6","uuid","regex",null].map(format=>({name:`strict format ${format}`,compat:{supportsStrictTools:true},context:{messages:[],tools:[{...tool,parameters:{type:"object",properties:{value:{type:"string",format}}},constrainedSampling:{type:"json_schema",strict:"require"}}]}})),
];
const cases=[];
for(const input of inputs){
 if(input.processCache)process.env.PI_CACHE_RETENTION=input.processCache;else delete process.env.PI_CACHE_RETENTION;
 const chosen={...model,...input.model,compat:input.compat};const compat=oracleCompat(chosen);
 const transcript=resolveTranscript(normalizeContext(input.context??context),compat.supportsMidConvoSystemMessages);
 const before=JSON.stringify({chosen,transcript,input});
 let params:any=null,error:string|null=null;
 try{params=oracleParams(chosen,transcript,input.oauth??false,input.options)}catch(failure){error=(failure as Error).message}
 if(JSON.stringify({chosen,transcript,input})!==before)throw new Error("Anthropic conversion mutated input");
 cases.push({input,expected:{compat,params,error}});
}
const file=new URL("pi-anthropic-params.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,context,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic request oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic transcript/tool/request cases against Pi ${manifest.commit}`);
