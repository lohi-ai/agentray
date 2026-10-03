import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {convertResponsesMessages,convertResponsesTools}=await import(new URL("api/openai-responses-shared.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {createGrammarToolInputProperties}=await import(new URL("api/constrained-sampling.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",name:"Test",api:"openai-responses",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text","image"],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},maxTokens:1000,contextWindow:10000};
const text=(text:string,textSignature?:string)=>({type:"text",text,textSignature});
const thinking=(thinking:string,thinkingSignature?:string)=>({type:"thinking",thinking,thinkingSignature});
const image={type:"image",mimeType:"image/png",data:"YQ=="};
const user=(content:any)=>({role:"user",content,timestamp:100});
const assistant=(content:any[],extra:any={})=>({role:"assistant",content,stopReason:"stop",timestamp:100,api:model.api,provider:model.provider,model:model.id,...extra});
const call=(id="call|fc_item",args:any={z:"<>&\u2028\u2029",a:1})=>({type:"toolCall",id,name:"tool",arguments:args});
const result=(id="call|fc_item",content:any[]=[text("result")])=>({role:"toolResult",toolCallId:id,toolName:"tool",content,isError:false,timestamp:100});
const tool={name:"tool",description:"Tool",parameters:{type:"object",properties:{z:{type:"string"},a:{type:"number"}}}};
const grammar={...tool,parameters:{type:"object",properties:{code:{type:"string"}},required:["code"]},constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}};
const updates=[{role:"system",content:"first",timestamp:100},user("hi"),{role:"system",content:"new",toolsAdded:[tool],sections:{scope:"latest"},timestamp:100},user("next")];
const inputs:any[]=[
 {name:"instructions",context:{systemPrompt:"system",messages:[user("hello")]}},
 {name:"nonreasoning",model:{reasoning:false},context:{systemPrompt:"system",messages:[user("")]}},
 ...[false,null,true].map(value=>({name:`developer role ${value}`,model:{compat:{supportsDeveloperRole:value}},context:{systemPrompt:"system",messages:[user("hi")]}})),
 {name:"exclude leading prompt",options:{includeSystemPrompt:false},context:{systemPrompt:"system",messages:[user("hi"),assistant([text("answer")])]}},
 {name:"user blocks",context:{messages:[user([text(""),image,text("hello")]),user([]),assistant([text("one"),text("two")])]}},
 {name:"nonvision",model:{input:["text"]},context:{messages:[user([image,image,text("hello")]),result("call",[image,image])]}},
 {name:"empty assistant skips index",context:{messages:[assistant([]),assistant([thinking("reason")]),assistant([text("")]),assistant([text("next")])]}},
 ...["legacy","",' {"v":1,"id":"msg_space"}',...Array.from({length:6},(_,i)=>JSON.stringify([{v:1,id:"msg_one",phase:"commentary"},{v:1,id:"msg_two",phase:"final_answer"},{v:1,id:"",phase:"final_answer"},{v:1,id:"msg_one",phase:"unknown"},{v:2,id:"wrong"},{v:1,id:42}][i])),"{"].map((signature,i)=>({name:`text signature ${i}`,context:{messages:[assistant([text("answer",signature),text("next")])]}})),
 ...["a".repeat(64),"a".repeat(65),"😀".repeat(33)].map((id,i)=>({name:`long text signature ${i}`,context:{messages:[assistant([text("answer",id)])]}})),
 {name:"reasoning signature",context:{messages:[assistant([thinking("summary",JSON.stringify({type:"reasoning",id:"rs_1",summary:[{type:"summary_text",text:"summary"}],encrypted_content:"opaque"})),text("answer")])]}},
 {name:"reasoning numeric semantics",context:{messages:[assistant([thinking("summary",'{"type":"reasoning","value":9007199254740993,"infinite":1e400}')])]}},
 ...["null","42","[]",'"text"'].map(signature=>({name:`reasoning unvalidated JSON ${signature}`,context:{messages:[assistant([thinking("summary",signature)])]}})),
 {name:"cross model signatures",context:{messages:[assistant([thinking("summary",'{"type":"reasoning"}'),text("answer","msg_old")],{model:"other"})]}},
 ...[{}, {model:"other"},{provider:"anthropic"},{api:"openai-completions"}].flatMap((source,i)=>["call|fc_item","call|ctc_item","call+/😀|rs_a+/=","call|", "!😀"+"x".repeat(80)+"__|item", "call|item|ignored"].map((id,j)=>({name:`tool id source ${i} variant ${j}`,context:{messages:[assistant([{...call(id),namespace:"ns"}],source),result(id)]}}))),
 {name:"disallowed provider",model:{provider:"custom"},context:{messages:[assistant([call("call+/|item+/")]),result("call+/|item+/")]}},
 {name:"same model null namespace",context:{messages:[assistant([{...call(),namespace:null}]),result()]}},
 {name:"tool arguments JS stringify",context:{messages:[assistant([call("call|fc_item",{"10":"ten","2":"two",z:"<>&\u2028",a:[-0,1e-7,1e21,1e-6]})]),result()]}},
 {name:"repair orphan and drop rejected",context:{messages:[assistant([call()]),user("next"),assistant([text("failed")],{stopReason:"error"}),assistant([text("aborted")],{stopReason:"aborted"})]}},
 {name:"tool image output",context:{messages:[result("a",[image]),result("b",[text(""),image]),result("c",[text("a"),text("b"),image]),result("d",[]),result("e",[text("")])]}},
 {name:"system folded",context:{messages:updates}},
 ...[{supportsMidConvoSystemMessages:true},{supportsMidConvoSystemMessages:true,supportsAdditionalTools:true},{supportsMidConvoSystemMessages:true,supportsToolSearch:true},{supportsMidConvoSystemMessages:true,supportsAdditionalTools:true,supportsToolSearch:true},{supportsMidConvoSystemMessages:true,supportsToolSearch:true,includeSystemPrompt:false},{supportsToolSearch:true}].map((options,i)=>({name:`system additions ${i}`,options,context:{messages:updates}})),
 ...["ctc_item","fc_item",""].map(id=>({name:`grammar replay ${id}`,tools:[grammar],options:{toolOptions:{supportsOpenAIGrammarTools:true}},context:{messages:[assistant([call("call|"+id,{code:"x < y\n"})]),result("call|"+id)]}})),
 {name:"grammar cross model",tools:[grammar],options:{toolOptions:{supportsOpenAIGrammarTools:true}},context:{messages:[assistant([call("call|ctc_item",{code:"x"})],{model:"other"})]}},
 {name:"grammar invalid argument",tools:[grammar],options:{toolOptions:{supportsOpenAIGrammarTools:true}},context:{messages:[assistant([call("call|ctc_item",{code:42})])]}},
 ...[{}, {strict:null},{strict:true},{strict:false},{supportsStrictMode:false},{supportsStrictMode:false,strict:true},{toolSearchResult:true}].map((toolOptions,i)=>({name:`tool defaults ${i}`,tools:[tool],options:{toolOptions},context:{messages:[]}})),
 ...[true,false].flatMap(supportsStrictMode=>["require","prefer"].map(strict=>({name:`tool strict ${strict} supported ${supportsStrictMode}`,tools:[{...tool,constrainedSampling:{type:"json_schema",strict}}],options:{toolOptions:{supportsStrictMode}},context:{messages:[]}}))),
 {name:"deferred grammar",tools:[grammar],options:{toolOptions:{supportsOpenAIGrammarTools:true,toolSearchResult:true}},context:{messages:[]}},
 {name:"grammar added through search",tools:[grammar],options:{supportsMidConvoSystemMessages:true,supportsToolSearch:true,toolOptions:{supportsOpenAIGrammarTools:true}},context:{messages:[user("hi"),{role:"system",toolsAdded:[grammar],timestamp:100}]}},
];
const allowed=["openai","openai-codex","opencode"];
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model};const before=JSON.stringify(input);
 let messages:any=null,tools:any=null,error:string|null=null;
 try{const options=input.options??{};const properties=createGrammarToolInputProperties(input.tools,options.toolOptions?.supportsOpenAIGrammarTools??false);messages=convertResponsesMessages(chosen,normalizeContext(input.context),new Set(allowed),{...options,grammarToolInputProperties:properties});tools=convertResponsesTools(input.tools??[],options.toolOptions)}catch(failure){error=(failure as Error).message}
 if(JSON.stringify(input)!==before)throw new Error("oracle mutated input");
 cases.push({input,expected:{messages,tools,error}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,model,allowed,cases},null,2)+"\n";
const file=new URL("pi-responses-messages.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Responses message oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Responses message/tool cases against Pi ${manifest.commit}`);
