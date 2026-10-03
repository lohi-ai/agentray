import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest,convertMessages,oracleCompat,oracleTools} from "./completions-oracle.ts";
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {createGrammarToolInputProperties}=await import(new URL("api/constrained-sampling.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",name:"Test",api:"openai-completions",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text","image"],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},maxTokens:1000,contextWindow:10000};
const text=(text:string)=>({type:"text",text});
const thinking=(thinking:string,thinkingSignature?:string)=>({type:"thinking",thinking,thinkingSignature});
const image={type:"image",mimeType:"image/png",data:"YQ=="};
const user=(content:any)=>({role:"user",content,timestamp:100});
const assistant=(content:any[],extra:any={})=>({role:"assistant",content,stopReason:"stop",timestamp:100,api:model.api,provider:model.provider,model:model.id,...extra});
const call=(id="call",args:any={z:"<>&\u2028\u2029\\u2028",a:1})=>({type:"toolCall",id,name:"tool",arguments:args});
const result=(id="call",content:any[]=[text("result")])=>({role:"toolResult",toolCallId:id,toolName:"tool",content,isError:false,timestamp:100});
const tool={name:"tool",description:"Tool",parameters:{type:"object",properties:{z:{type:"string"},a:{type:"number"}}}};
const grammar={...tool,parameters:{type:"object",properties:{code:{type:"string"}},required:["code"]},constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}};
const inputs:any[]=[
 {name:"instructions and user",context:{systemPrompt:"system",messages:[user("hello")]}},
 {name:"nonreasoning system",model:{reasoning:false},context:{systemPrompt:"system",messages:[user("")]}},
 {name:"user images and empty text",context:{messages:[user([text(""),image,text("hello")]),user([])]}},
 {name:"nonvision images",model:{input:["text"]},context:{messages:[user([image,image,text("hello"),image]),result("call",[image])]}},
 {name:"assistant text parts",context:{messages:[assistant([text(" "),text("one"),text("two")]),assistant([])]}},
 {name:"raw thinking",context:{messages:[assistant([thinking("reason","reasoning_content"),thinking("more"),text("answer")])]}},
 {name:"thinking as text",compat:{requiresThinkingAsText:true},context:{messages:[assistant([thinking("reason","reasoning"),thinking("more"),text("answer")])]}},
 {name:"thinking only skipped",context:{messages:[assistant([thinking("reason","reasoning")])]}},
 {name:"opencode reasoning field",model:{provider:"opencode-go"},context:{messages:[assistant([thinking("reason","reasoning"),text("answer")],{provider:"opencode-go"})]}},
 {name:"deepseek empty reasoning",model:{provider:"deepseek"},context:{messages:[assistant([text("answer")],{provider:"deepseek"})]}},
 {name:"valid structured reasoning",context:{messages:[assistant([thinking("reason",JSON.stringify([{type:"reasoning.text",text:"raw",signature:null,id:null,index:0,format:"test",opaque:{exact:"kept"}},{type:"reasoning.encrypted",data:"secret",id:"e"}])),call()]),result()]}},
 {name:"structured reasoning JSON number semantics",context:{messages:[assistant([thinking("reason",'[{"type":"reasoning.text","text":"raw","opaque":9007199254740993}]'),text("answer")])]}},
 {name:"structured reasoning infinite parsed number",context:{messages:[assistant([thinking("reason",'[{"type":"reasoning.text","text":"raw","index":1e400}]'),text("answer")])]}},
 {name:"legacy signature JSON number semantics",context:{messages:[assistant([{...call(),thoughtSignature:'{"type":"reasoning.encrypted","id":"e","data":"secret","opaque":9007199254740993}'}]),result()]}},
 {name:"invalid structured reasoning",context:{messages:[assistant([thinking("reason",JSON.stringify([{type:"reasoning.text",text:"raw",index:null}])),text("answer")])]}},
 ...[[],{type:"reasoning.text",text:"object"},[{type:"unknown",text:"raw"}],[{type:"reasoning.summary",summary:3}],[{type:"reasoning.text",text:"raw",signature:{}}],[{type:"reasoning.text",text:"raw",index:"0"}],[{type:"reasoning.text",text:"raw",id:3}],[{type:"reasoning.text",text:"raw",format:null}]].map((signature,i)=>({name:`invalid reasoning signature ${i}`,context:{messages:[assistant([thinking("reason",JSON.stringify(signature)),text("answer")])]}})),
 {name:"reasoning text field",context:{messages:[assistant([thinking("reason","reasoning_text"),text("answer")])]}},
 {name:"cross model thinking becomes text",context:{messages:[assistant([thinking("reason","reasoning_content"),text("answer")],{model:"other"})]}},
 {name:"legacy encrypted invalid signature",context:{messages:[assistant([{...call(),thoughtSignature:JSON.stringify({type:"reasoning.encrypted",id:"",data:"secret"})}]),result()]}},
 {name:"legacy encrypted reasoning",context:{messages:[assistant([{...call(),thoughtSignature:JSON.stringify({type:"reasoning.encrypted",id:"e",data:"secret",extra:7})}]),result()]}},
 {name:"structured details win legacy",context:{messages:[assistant([thinking("",JSON.stringify([{type:"reasoning.summary",summary:"saved"}])),{...call(),thoughtSignature:JSON.stringify({type:"reasoning.encrypted",id:"e",data:"secret"})}]),result()]}},
 {name:"same model preserves long id",context:{messages:[assistant([call("x".repeat(70))]),result("x".repeat(70))]}},
 {name:"cross model id truncation",context:{messages:[assistant([call("x".repeat(70))],{model:"other"}),result("x".repeat(70))]}},
 {name:"cross model pipe id uniqueness",context:{messages:[assistant([call("same|one+/="),call("same|two+/="),call("same|"+"x".repeat(100))],{api:"openai-responses"}),result("same|one+/="),result("same|two+/="),result("same|"+"x".repeat(100))]}},
 {name:"cross model unicode hash",context:{messages:[assistant([call("😀call|"+"😀".repeat(40))],{api:"openai-responses"})]}},
 {name:"tool argument stringify",context:{messages:[assistant([call("call",{"10":"ten","2":"two",z:"<>&\u2028",a:[-0,1e-7,1e21,1e-6]})]),result()]}},
 {name:"orphan repair and rejected assistant",context:{messages:[assistant([call()]),user("next"),assistant([text("failed")],{stopReason:"error"}),assistant([text("stopped")],{stopReason:"aborted"})]}},
 {name:"tool images grouped",context:{messages:[assistant([call("one"),call("two")]),result("one",[image]),result("two",[text("one"),text("two"),image]),user("next")]}},
 {name:"tool images bridge and name",compat:{requiresAssistantAfterToolResult:true,requiresToolResultName:true},context:{messages:[assistant([call("one"),call("two")]),result("one",[image]),result("two",[]),user("next")]}},
 {name:"tool text bridge",compat:{requiresAssistantAfterToolResult:true},context:{messages:[assistant([call()]),result(),user("next")]}},
 {name:"mid conversation instructions collapsed",context:{messages:[{role:"system",timestamp:100,content:"first"},user("hi"),{role:"system",timestamp:100,content:"second",sections:{scope:"new"}},user("next")]}},
 {name:"mid conversation tools anchored",compat:{supportsMidConvoSystemMessages:true,supportsMidConvoToolAdditions:true},context:{messages:[{role:"system",timestamp:100,content:"first"},user("hi"),{role:"system",timestamp:100,content:"second",toolsAdded:[tool],sections:{scope:null}},user("next")]}},
 {name:"grammar tools and calls",compat:{supportsOpenAIGrammarTools:true},tools:[grammar],context:{messages:[assistant([call("call",{code:"x < y\n"})]),result()]}},
 {name:"invalid grammar argument",compat:{supportsOpenAIGrammarTools:true},tools:[grammar],context:{messages:[assistant([call("call",{code:42})])]}},
 {name:"strict tool",compat:{supportsStrictMode:true},tools:[{...tool,constrainedSampling:{type:"json_schema",strict:"require"}}],context:{messages:[]}},
 {name:"strict required unsupported",tools:[{...tool,constrainedSampling:{type:"json_schema",strict:"require"}}],context:{messages:[]}},
 {name:"strict optional unsupported schema",compat:{supportsStrictMode:true},tools:[{...tool,parameters:{type:"object",oneOf:[]},constrainedSampling:{type:"json_schema",strict:"prefer"}}],context:{messages:[]}},
 ...["zai","zai-coding-cn","together","moonshotai","openrouter","cloudflare-workers-ai","cloudflare-ai-gateway","nvidia","ant-ling","cerebras","deepseek","xai","opencode"].map(provider=>({name:`compat ${provider}`,model:{provider},context:{messages:[]}})),
 ...["https://api.z.ai","https://api.together.xyz","https://api.moonshot.cn","https://openrouter.ai","https://api.cloudflare.com","https://gateway.ai.cloudflare.com","https://integrate.api.nvidia.com","https://api.ant-ling.com","https://cerebras.ai","https://DEEPSEEK.COM","https://api.x.ai","https://opencode.ai","https://chutes.ai"].map(baseUrl=>({name:`compat url ${baseUrl}`,model:{provider:"custom",baseUrl},context:{messages:[]}})),
 {name:"router anthropic",model:{id:"anthropic/model",provider:"openrouter"},context:{messages:[]}},
 {name:"router openai",model:{id:"openai/model",provider:"openrouter"},context:{messages:[]}},
 {name:"nullish and explicit overrides",compat:{supportsStore:false,supportsFinishReason:false,maxTokensField:"max_tokens",supportsReasoningEffort:null,thinkingFormat:"custom",openRouterRouting:{order:["one"]},vllmPriority:null},context:{messages:[]}},
];
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model,compat:input.compat};
 const before=JSON.stringify(input);
 const compat=oracleCompat(chosen);
 let messages:any=null,tools:any=null,error:string|null=null;
 try{const properties=createGrammarToolInputProperties(input.tools,compat.supportsOpenAIGrammarTools);messages=convertMessages(chosen,normalizeContext(input.context),compat,{grammarToolInputProperties:properties});tools=oracleTools(input.tools??[],compat)}catch(failure){error=(failure as Error).message}
 if(JSON.stringify(input)!==before)throw new Error("oracle mutated input");
 cases.push({input,expected:{compat,messages,tools,error}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,model,cases},null,2)+"\n";
const file=new URL("pi-completions-messages.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Completions message oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} completions message/tool/compat cases against Pi ${manifest.commit}`);
