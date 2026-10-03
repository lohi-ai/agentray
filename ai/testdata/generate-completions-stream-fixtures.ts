import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest,stream} from "./completions-oracle.ts";
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",name:"Test",api:"openai-completions",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text"],maxTokens:20000,contextWindow:100000,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2}};
const tool={name:"code",description:"Code",parameters:{type:"object",properties:{source:{type:"string"}},required:["source"]},constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}};
const chunk=(delta:any={},finish_reason:any=null,extra:any={})=>({id:"response",choices:[{index:0,delta,finish_reason}],...extra});
const call=(index:any,id:any,name:any,args:any)=>({index,id,function:{name,arguments:args}});
const custom=(index:any,id:any,name:any,input:any)=>({index,id,custom:{name,input}});
const tools=(...tool_calls:any[])=>chunk({tool_calls});
const text=(content:string)=>chunk({content});
const end=chunk({},"stop");
const detail=(...reasoning_details:any[])=>chunk({reasoning_details});
const usage=(value:any)=>({choices:[],usage:value});
const inputs:any[]=[
 {name:"empty successful",chunks:[end]},
 {name:"empty missing finish",chunks:[]},
 {name:"interleaved text thinking tools",chunks:[chunk({content:"hello",reasoning_content:"think",reasoning:"duplicate",tool_calls:[call(0,"c","tool",'{"v":')]}),chunk({reasoning_text:" more",content:" world"}),tools(call(0,undefined,undefined,"1}")),chunk({},"tool_calls")]},
 {name:"id index late admission",chunks:[tools(call(undefined,"a",undefined,"{")),tools(call(9,"a","tool",'"value":')),tools(call(9,"alias",undefined,"4}")),tools(call(undefined,"alias",undefined," ")),chunk({},"tool_calls")]},
 {name:"distinct tools interleaved",chunks:[tools(call(8,"a","one",'{"a":')),tools(call(2,"b","two",'[1,')),tools(call(8,undefined,"renamed","2}"),call(2,undefined,undefined,"2]")),chunk({},"function_call")]},
 {name:"index wins over id",chunks:[tools(call(0,"a","one","{}"),call(1,"b","two","[]")),tools(call(0,"b",undefined," ")),tools(call(undefined,"b",undefined," ")),end]},
 {name:"new index does not replace admitted index",chunks:[tools(call(0,"a","one","{}")),tools(call(1,"a",undefined," ")),tools(call(1,undefined,"two","[]")),end]},
 {name:"anonymous calls remain separate",chunks:[tools(call(undefined,undefined,"one","{}")),tools(call(undefined,undefined,"two","null")),end]},
 {name:"empty tool delta",chunks:[tools(call(0,"a","tool","")),end]},
 {name:"incomplete repaired tool json",chunks:[tools(call(0,"a","tool",'{"a":"hel')),tools(call(0,undefined,undefined,'lo')),end]},
 {name:"custom grammar escapes",compat:{supportsOpenAIGrammarTools:true},chunks:[tools(custom(0,"c","code","x = \"hello\"\n")),tools(custom(0,undefined,undefined,"<>&\u2028😀")),chunk({},"tool_calls")]},
 {name:"custom unknown fallback",chunks:[tools(custom(0,"c","unknown","raw")),end]},
 {name:"custom empty closes",compat:{supportsOpenAIGrammarTools:true},chunks:[tools(custom(0,"c","code","")),end]},
 {name:"function becomes custom",compat:{supportsOpenAIGrammarTools:true},chunks:[tools(call(0,"c",undefined,"{")),tools(custom(0,undefined,"code","raw")),end]},
 {name:"function preferred when both fields present",chunks:[tools({...custom(0,"c","code","wrong"),function:{name:"function",arguments:"{}"}}),end]},
 {name:"custom then function arguments",compat:{supportsOpenAIGrammarTools:true},chunks:[tools(custom(0,"c","code","first")),tools(call(0,undefined,undefined,'{"source":"reset"}')),end]},
 {name:"custom mismatch during chunk",compat:{supportsOpenAIGrammarTools:true},chunks:[tools(custom(0,"c","code","first")),tools(call(0,undefined,undefined,'{"source":"reset"}')),tools(custom(0,undefined,undefined,"suffix")),end]},
 {name:"reasoning aliases priority",chunks:[chunk({reasoning_content:"",reasoning:"a",reasoning_text:"duplicate"}),chunk({reasoning_content:"b"}),end]},
 {name:"opencode signature",model:{provider:"opencode-go"},chunks:[chunk({reasoning:"think"}),end]},
 {name:"reasoning metadata coalesces",chunks:[detail({type:"reasoning.text",text:"one",signature:null,id:null,format:"",extra:{z:1}}),detail({type:"reasoning.text",text:" two",signature:"sig",id:"id",format:"fmt",index:0,extra:"ignored"}),detail({type:"reasoning.summary",summary:"first"},{type:"reasoning.summary",summary:" second",id:"s"}),detail({type:"reasoning.encrypted",data:"opaque"},{type:"reasoning.encrypted",data:"opaque2"}),end]},
 {name:"reasoning signature property order",chunks:[detail({type:"reasoning.text",text:"a",signature:null,id:null}),detail({type:"reasoning.text",text:"b"}),detail({type:"reasoning.text",text:"c",index:2,format:"fmt",id:"id",signature:"sig"}),end]},
 {name:"reasoning undefined keys keep insertion order",chunks:[detail({type:"reasoning.text",text:"a"}),detail({type:"reasoning.text",text:"b",index:2}),detail({type:"reasoning.text",text:"c",signature:"sig",id:"id",format:"fmt"}),end]},
 {name:"metadata before visible reasoning",chunks:[detail({type:"reasoning.text",text:"metadata"}),chunk({reasoning:"visible"}),end]},
 {name:"visible reasoning before metadata",chunks:[chunk({reasoning:"visible"}),detail({type:"reasoning.text",text:"metadata"}),end]},
 {name:"invalid details ignored",chunks:[detail(null,{},"bad",{type:"reasoning.text",text:1},{type:"reasoning.summary",summary:"ok",index:"0"}),end]},
 {name:"details preserve unknown and numeric semantics",chunks:[detail({type:"reasoning.text",text:"<>&\u2028",metadata:{"2":"two","1":"one",huge:9007199254740993},index:0}),end]},
 {name:"response model first mismatch",chunks:[chunk({},null,{model:"test"}),chunk({content:"a"},null,{id:"later",model:"actual"}),chunk({},"stop",{model:"another"})]},
 {name:"empty response id then absent",chunks:[chunk({content:"a"},null,{id:""}),{choices:[{finish_reason:"stop"}]}]},
 {name:"null response id then absent",chunks:[chunk({content:"a"},null,{id:null}),{choices:[{finish_reason:"stop"}]}]},
 {name:"null response id",chunks:[chunk({content:"a"},"stop",{id:null})]},
 {name:"choice usage fallback",chunks:[{choices:[{delta:{},finish_reason:"stop",usage:{prompt_tokens:20,completion_tokens:8,prompt_cache_hit_tokens:4,completion_tokens_details:{reasoning_tokens:2}}}]}]},
 {name:"top usage takes precedence",chunks:[{choices:[{finish_reason:"stop",usage:{prompt_tokens:999}}],usage:{prompt_tokens:30,completion_tokens:5,prompt_tokens_details:{cached_tokens:7,cache_write_tokens:3}}}]},
 {name:"usage after finish replaces",chunks:[chunk({},"stop",{usage:{prompt_tokens:10,completion_tokens:1}}),usage({prompt_tokens:50,completion_tokens:20,cached_tokens:12})]},
 {name:"cache zero beats fallbacks",chunks:[end,usage({prompt_tokens:10,prompt_tokens_details:{cached_tokens:0},prompt_cache_hit_tokens:3,cached_tokens:4})]},
 {name:"cache null falls back",chunks:[end,usage({prompt_tokens:10,prompt_tokens_details:{cached_tokens:null},prompt_cache_hit_tokens:null,cached_tokens:4})]},
 {name:"cache exceeds prompt",chunks:[end,usage({prompt_tokens:1,prompt_tokens_details:{cached_tokens:3,cache_write_tokens:2}})]},
 {name:"tier largest strict threshold",model:{cost:{...model.cost,tiers:[{inputTokensAbove:20,input:10,output:20,cacheRead:3,cacheWrite:4},{inputTokensAbove:5,input:2,output:4,cacheRead:0.2,cacheWrite:3},{inputTokensAbove:20,input:999,output:999,cacheRead:999,cacheWrite:999}]}},chunks:[end,usage({prompt_tokens:21,completion_tokens:2,prompt_tokens_details:{cached_tokens:3,cache_write_tokens:2}})]},
 {name:"tier equal threshold excluded",model:{cost:{...model.cost,tiers:[{inputTokensAbove:20,input:10,output:20,cacheRead:3,cacheWrite:4}]}},chunks:[end,usage({prompt_tokens:20,completion_tokens:2})]},
 ...["stop","end","length","function_call","tool_calls","content_filter","network_error","unknown"].map(reason=>({name:`finish ${reason}`,chunks:[chunk({content:"answer"},reason)]})),
 {name:"missing finish preserves block ends",chunks:[text("answer")]},
 {name:"optional finish text",compat:{supportsFinishReason:false},chunks:[text("answer")]},
 {name:"optional finish tool",compat:{supportsFinishReason:false},chunks:[tools(call(0,"c","tool","{}"))]},
 {name:"optional finish empty",compat:{supportsFinishReason:false},chunks:[]},
 {name:"later finish leaves earlier error message",chunks:[chunk({},"content_filter"),end]},
 {name:"callback failure strips scratch without end",failBefore:1,chunks:[tools(call(0,"c","tool",'{"x":')),end]},
 {name:"callback failure applies metadata",failBefore:2,chunks:[detail({type:"reasoning.text",text:"meta"}),tools(custom(0,"c","code","raw")),end]},
 {name:"callback failure before first chunk",failBefore:0,chunks:[end]},
 {name:"abort after block end",abortAtEnd:true,chunks:[text("answer"),end]},
];
const originalPush=AssistantMessageEventStream.prototype.push;
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model,compat:input.compat};
 const events:any[]=[];
 const live:any[]=[];
 const controller=new AbortController();
 AssistantMessageEventStream.prototype.push=function(event:any){
  events.push(JSON.parse(JSON.stringify(event)));live.push(event);
  if(input.abortAtEnd&&event.type==="text_end")controller.abort();
  return originalPush.call(this,event);
 };
 let received=0;
 const resultStream=stream(chosen,normalizeContext({messages:[],tools:[tool]}),{
  apiKey:"fixture-key",signal:controller.signal,maxRetries:0,
  fetch:async()=>new Response(input.chunks.map((value:any)=>`data: ${JSON.stringify(value)}\n\n`).join("")+"data: [DONE]\n\n",{headers:{"content-type":"text/event-stream"}}),
  onProviderStreamEvent:()=>{if(received++===input.failBefore)throw new Error("fixture failure")},
 });
 const result=await resultStream.result();
 for await(const _ of resultStream){}
 for(const event of live){if(event.partial&&event.partial!==result)throw new Error("original lost partial identity")}
 cases.push({input,expected:{events,result}});
}
AssistantMessageEventStream.prototype.push=originalPush;
const file=new URL("pi-completions-stream.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,tool,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Completions stream oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} completions stream cases against Pi ${manifest.commit}`);
