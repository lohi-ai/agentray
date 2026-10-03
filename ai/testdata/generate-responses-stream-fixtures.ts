import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/openai-responses.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"test",name:"Test",api:"openai-responses",provider:"openai",baseUrl:"https://api.openai.com/v1",reasoning:true,input:["text"],maxTokens:20000,contextWindow:100000,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2}};
const tool={name:"code",description:"Code",parameters:{type:"object",properties:{source:{type:"string"}},required:["source"]},constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}};
const message=(text="answer",extra:any={})=>({type:"message",id:"msg_1",role:"assistant",status:"completed",content:[{type:"output_text",text,annotations:[]}],...extra});
const reasoning=(extra:any={})=>({type:"reasoning",id:"rs_1",summary:[{type:"summary_text",text:"summary"}],...extra});
const call=(args="{}",extra:any={})=>({type:"function_call",id:"fc_1",call_id:"call",name:"tool",arguments:args,...extra});
const custom=(input="",extra:any={})=>({type:"custom_tool_call",id:"ctc_1",call_id:"call",name:"code",input,...extra});
const added=(item:any,index:any=0)=>({type:"response.output_item.added",output_index:index,item});
const done=(item:any,index:any=0)=>({type:"response.output_item.done",output_index:index,item});
const delta=(type:string,delta:any,index:any=0)=>({type:`response.${type}.delta`,output_index:index,delta});
const terminal=(response:any={})=>({type:"response.completed",response:{id:"response",status:"completed",output:[],...response}});
const usage={input_tokens:20,output_tokens:8,total_tokens:28,input_tokens_details:{cached_tokens:4,cache_write_tokens:2},output_tokens_details:{reasoning_tokens:3}};
const end=terminal();
const inputs:any[]=[
 {name:"empty success",chunks:[end]},
 {name:"empty stream missing terminal",chunks:[]},
 {name:"text delta then done",chunks:[added(message()),delta("output_text","hello"),delta("output_text"," world"),done(message("final")),end]},
 {name:"text done only",chunks:[done(message()),end]},
 {name:"text empty done overrides deltas",chunks:[added(message()),delta("output_text","ignored"),done(message("")),end]},
 {name:"refusal text",chunks:[added(message()),delta("refusal","cannot"),done(message("",{content:[{type:"refusal",refusal:"cannot answer"}]})),end]},
 {name:"interleaved slots",chunks:[added(reasoning(),7),added(message(),2),added(call(),9),delta("reasoning_summary_text","think",7),delta("output_text","answer",2),delta("function_call_arguments",'{"x":',9),{type:"response.reasoning_summary_part.done",output_index:7},delta("reasoning_text","raw",7),delta("function_call_arguments","1}",9),done(message("answer"),2),done(call('{"x":1}'),9),done(reasoning(),7),end]},
 {name:"wrong type and missing slot deltas ignored",chunks:[delta("output_text","ignored",9),added(message()),delta("reasoning_text","ignored"),delta("function_call_arguments","ignored"),delta("custom_tool_call_input","ignored"),done(message()),end]},
 {name:"reasoning content fallback",chunks:[added(reasoning()),delta("reasoning_text","delta"),done(reasoning({summary:[],content:[{type:"reasoning_text",text:"one"},{type:"reasoning_text",text:"two"}]})),end]},
 {name:"reasoning delta fallback",chunks:[added(reasoning()),delta("reasoning_text","delta"),done(reasoning({summary:[],content:[]})),end]},
 {name:"reasoning summary preferred",chunks:[done(reasoning({summary:[{text:"one"},{text:"two"}],content:[{text:"raw"}],encrypted_content:"encrypted",opaque:{z:"<&>\u2028",a:1}})),end]},
 ...[undefined,"",null,"old"].map(encrypted_content=>({name:`reasoning backfill ${encrypted_content??String(encrypted_content)}`,chunks:[done(reasoning({encrypted_content})),terminal({output:[reasoning({encrypted_content:"new"})]})]})),
 {name:"reasoning backfill unmatched",chunks:[done(reasoning()),terminal({output:[reasoning({id:"other",encrypted_content:"new"})]})]},
 {name:"reasoning terminal alone creates no block",chunks:[terminal({output:[reasoning({encrypted_content:"new"})]})]},
 ...["commentary","final_answer",null,""].map(phase=>({name:`phase ${phase}`,chunks:[added(message("",{phase})),delta("output_text","answer"),done(message("answer",{phase})),end]})),
 {name:"phase affects state before terminal",chunks:[done(call()),done(message("final",{phase:"final_answer"}),1),end]},
 {name:"function initial arguments are scratch",chunks:[added(call('{"x":1}')),done(call("")),end]},
 {name:"function done creates slot",chunks:[done(call('{"x":2}')),end]},
 {name:"function arguments done emits suffix",chunks:[added(call("")),delta("function_call_arguments",'{"x":'),{type:"response.function_call_arguments.done",output_index:0,arguments:'{"x":1}'},done(call('{"x":1}')),end]},
 {name:"function arguments done replacement silent",chunks:[added(call("")),delta("function_call_arguments",'{"x":'),{type:"response.function_call_arguments.done",output_index:0,arguments:'{"y":2}'},done(call("")),end]},
 {name:"function arguments done identical silent",chunks:[added(call("{}")),{type:"response.function_call_arguments.done",output_index:0,arguments:"{}"},done(call()),end]},
 {name:"function empty arguments fallback",chunks:[done(call("")),end]},
 {name:"function partial JSON repair",chunks:[added(call("")),delta("function_call_arguments",'{"x":"hel'),done(call("")),end]},
 {name:"function namespace final overwrites",chunks:[added(call("",{namespace:"initial"})),done(call("{}",{namespace:"final"})),end]},
 {name:"function namespace null final",chunks:[added(call("",{namespace:"initial"})),done(call("{}",{namespace:null})),end]},
 {name:"function namespace retained",chunks:[added(call("",{namespace:null})),done(call()),end]},
 {name:"custom grammar escapes",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom()),delta("custom_tool_call_input","x = \"hello\"\n"),delta("custom_tool_call_input","<>&\u2028😀"),{type:"response.custom_tool_call_input.done",output_index:0,input:"x = \"hello\"\n<>&\u2028😀"},done(custom("x = \"hello\"\n<>&\u2028😀")),end]},
 {name:"custom initial input emitted on next delta",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom("prefix")),delta("custom_tool_call_input","suffix"),done(custom("prefixsuffix")),end]},
 {name:"custom fallback property",chunks:[done(custom("raw",{name:"unknown"})),end]},
 {name:"custom empty closes",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom()),done(custom()),end]},
 {name:"custom final input missing uses retained",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom("prefix")),done(custom(undefined,{input:undefined,namespace:"ns"})),end]},
 {name:"custom final null uses retained",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom("prefix")),done(custom("",{input:null,namespace:null})),end]},
 {name:"custom nonmonotonic input",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom()),delta("custom_tool_call_input","prefix"),done(custom("other")),end]},
 {name:"custom change after closed",compat:{supportsOpenAIGrammarTools:true},chunks:[added(custom()),{type:"response.custom_tool_call_input.done",output_index:0,input:"raw"},delta("custom_tool_call_input","changed"),end]},
 {name:"custom function deltas ignored",chunks:[added(custom()),delta("function_call_arguments","{}"),{type:"response.function_call_arguments.done",output_index:0,arguments:"{}"},done(custom("raw")),end]},
 {name:"function custom deltas ignored",chunks:[added(call()),delta("custom_tool_call_input","raw"),{type:"response.custom_tool_call_input.done",output_index:0,input:"raw"},done(call()),end]},
 {name:"unfinished function rejected",chunks:[added(call()),end]},
 {name:"unfinished custom rejected",chunks:[added(custom()),end]},
 {name:"unfinished function allowed on length",chunks:[added(call()),terminal({status:"incomplete",incomplete_details:{reason:"max_output_tokens"}})]},
 {name:"text unfinished terminal accepted",chunks:[added(message()),delta("output_text","unfinished"),end]},
 {name:"duplicate added leaves unfinished call",chunks:[added(call()),added(call("{}",{id:"fc_2"})),done(call("{}",{id:"fc_2"})),end]},
 {name:"mismatched done leaves unfinished call",chunks:[added(call()),done(message()),end]},
 {name:"duplicate done creates second block",chunks:[done(message("one")),done(message("two")),end]},
 {name:"unknown item ignored",chunks:[added({type:"web_search_call",id:"w"}),done({type:"web_search_call",id:"w"}),end]},
 {name:"missing indexes collide",chunks:[{...added(call()),output_index:undefined},{...added(call("{}",{id:"fc_2"})),output_index:undefined},{...done(call()),output_index:undefined},end]},
 {name:"usage and cache subtraction",chunks:[terminal({usage})]},
 {name:"cache exceeds input",chunks:[terminal({usage:{input_tokens:1,input_tokens_details:{cached_tokens:4,cache_write_tokens:2}}})]},
 {name:"empty usage",chunks:[terminal({usage:{}})]},
 {name:"usage absent retains last",chunks:[terminal({usage}),end]},
 {name:"usage replaced",chunks:[terminal({usage}),terminal({usage:{input_tokens:2,output_tokens:1,total_tokens:999}})]},
 ...["flex","priority","fast","default"].flatMap(serviceTier=>["test","gpt-5.5"].map(id=>({name:`pricing ${serviceTier} ${id}`,model:{id},options:{serviceTier},chunks:[terminal({usage})]}))),
 {name:"response tier overrides request",options:{serviceTier:"flex"},chunks:[terminal({usage,service_tier:"priority"})]},
 {name:"null response tier falls back",options:{serviceTier:"flex"},chunks:[terminal({usage,service_tier:null})]},
 {name:"tiered pricing",model:{cost:{...model.cost,tiers:[{inputTokensAbove:20,input:9,output:9,cacheRead:9,cacheWrite:9},{inputTokensAbove:19,input:2,output:4,cacheRead:0.2,cacheWrite:3}]}},options:{serviceTier:"priority"},chunks:[terminal({usage})]},
 ...[undefined,null,"", "completed","in_progress","queued","failed","cancelled","unknown"].map(status=>({name:`terminal status ${status??String(status)}`,chunks:[terminal({status})]})),
 ...[undefined,null,"", "max_output_tokens","content_filter",42].map(reason=>({name:`incomplete reason ${reason??String(reason)}`,chunks:[{type:"response.incomplete",response:{status:"incomplete",incomplete_details:{reason}}}]})),
 {name:"completed clears earlier error",chunks:[terminal({status:"incomplete",incomplete_details:{reason:"content_filter"}}),end]},
 ...[{error:{code:"quota",message:"exhausted"}},{error:{}},{incomplete_details:{reason:"timeout"}},{}].map((extra,i)=>({name:`failed response ${i}`,chunks:[{type:"response.failed",response:{status:"failed",...extra}}]})),
 {name:"error event",chunks:[{type:"error",code:"invalid",message:"bad input"}]},
 {name:"error event no fields",chunks:[{type:"error"}]},
 {name:"subscription sharing link",chunks:[{type:"response.failed",response:{status:"failed",error:{code:"subscription_sharing_usage_limit_exceeded",message:"limit"}}}]},
 {name:"missing terminal text",chunks:[done(message())]},
 {name:"created id overwritten",chunks:[{type:"response.created",response:{id:"initial"}},terminal({id:"final"})]},
 {name:"empty terminal id retained",chunks:[{type:"response.created",response:{id:"initial"}},terminal({id:""})]},
 {name:"created null id",chunks:[{type:"response.created",response:{id:null}},terminal({id:undefined})]},
 {name:"callback failure before event",failBefore:0,chunks:[end]},
 {name:"callback failure strips function scratch",failBefore:1,chunks:[added(call()),end]},
 {name:"callback failure strips custom scratch",failBefore:2,chunks:[added(custom()),delta("custom_tool_call_input","raw"),end]},
 {name:"abort after text end",abortAtEnd:true,chunks:[done(message()),end]},
];
const originalPush=AssistantMessageEventStream.prototype.push;
const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model,compat:input.compat};
 const events:any[]=[];const live:any[]=[];const controller=new AbortController();
 AssistantMessageEventStream.prototype.push=function(event:any){events.push(JSON.parse(JSON.stringify(event)));live.push(event);if(input.abortAtEnd&&event.type==="text_end")controller.abort();return originalPush.call(this,event)};
 let received=0;
 const resultStream=stream(chosen,normalizeContext({messages:[],tools:[tool]}),{...input.options,
 apiKey:"sk-fixture",signal:controller.signal,maxRetries:0,
 fetch:async()=>new Response(input.chunks.map((value:any)=>`data: ${JSON.stringify(value)}\n\n`).join("")+"data: [DONE]\n\n",{headers:{"content-type":"text/event-stream"}}),
 onProviderStreamEvent:()=>{if(received++===input.failBefore)throw new Error("fixture failure")},
 });
 const result=await resultStream.result();for await(const _ of resultStream){}
 for(const event of live){if(event.partial&&event.partial!==result)throw new Error("original lost partial identity")}
 cases.push({input,expected:{events,result}});
}
AssistantMessageEventStream.prototype.push=originalPush;
const file=new URL("pi-responses-stream.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,model,tool,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Responses stream oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Responses stream cases against Pi ${manifest.commit}`);
