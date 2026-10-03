import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
const {AssistantMessageEventStream}=await import(new URL("utils/event-stream.ts",aiRoot).pathname);
Date.now=()=>100;
const model={id:"claude-test",name:"Test",api:"anthropic-messages",provider:"anthropic",baseUrl:"https://api.anthropic.com",reasoning:true,input:["text","image"],maxTokens:8192,contextWindow:200000,cost:{input:1,output:3,cacheRead:0.1,cacheWrite:2}};
const begin=(message:any={})=>({type:"message_start",message:{id:"response",model:model.id,usage:{input_tokens:10,output_tokens:1,cache_read_input_tokens:2,cache_creation_input_tokens:3},...message}});
const start=(content_block:any,index:any=0)=>({type:"content_block_start",index,content_block});
const delta=(delta:any,index:any=0)=>({type:"content_block_delta",index,delta});
const stop=(index:any=0)=>({type:"content_block_stop",index});
const finish=(stop_reason:any="end_turn",extra:any={})=>({type:"message_delta",delta:{stop_reason},...extra});
const end={type:"message_stop"};
const text=(value="")=>start({type:"text",text:value});
const textDelta=(text:string,index=0)=>delta({type:"text_delta",text},index);
const thinking=(value="",signature:any="")=>start({type:"thinking",thinking:value,signature});
const tool=(input:any={},index=0,name="read")=>start({type:"tool_use",id:"call",name,input},index);
const args=(partial_json:string,index=0)=>delta({type:"input_json_delta",partial_json},index);
const frame=(chunk:any)=>`event: ${chunk.type}\ndata: ${JSON.stringify(chunk)}\n\n`;
const declaration=(name:string)=>({name,description:name,parameters:{type:"object",properties:{}}});
const success=[begin(),text(),textDelta("hello"),stop(),finish(),end];
const inputs:any[]=[
 {name:"success",chunks:success},
 {name:"empty no stop",chunks:[]},
 {name:"message stop without stop reason",chunks:[begin(),end]},
 {name:"missing message stop",chunks:[begin(),text(),textDelta("partial"),finish()]},
 {name:"no start permits final delta",chunks:[finish()]},
 {name:"start usage null and omitted",chunks:[begin({usage:{input_tokens:null,output_tokens:0}}),finish(),end]},
 {name:"text initial content",chunks:[begin(),text("prefix"),textDelta("suffix"),stop(),finish(),end]},
 {name:"thinking signature concatenated",chunks:[begin(),thinking("initial","sig"),delta({type:"thinking_delta",thinking:" more"}),delta({type:"signature_delta",signature:"nature"}),stop(),finish(),end]},
 {name:"thinking signature null start",chunks:[begin(),thinking("reason",null),delta({type:"signature_delta",signature:"sig"}),stop(),finish(),end]},
 {name:"redacted thinking",chunks:[begin(),start({type:"redacted_thinking",data:"opaque"}),stop(),finish(),end]},
 {name:"redacted signature extends",chunks:[begin(),start({type:"redacted_thinking",data:"opaque"}),delta({type:"thinking_delta",thinking:" extra"}),delta({type:"signature_delta",signature:"tail"}),stop(),finish(),end]},
 {name:"redacted null signature",chunks:[begin(),start({type:"redacted_thinking",data:null}),stop(),finish(),end]},
 {name:"tool initial input overwritten at end",chunks:[begin(),tool({initial:1}),stop(),finish("tool_use"),end]},
 {name:"tool argument delta repair",chunks:[begin(),tool(),args('{"file":"hel'),args('lo'),stop(),finish("tool_use"),end]},
 {name:"tool input null defaults",chunks:[begin(),tool(null),stop(),finish("tool_use"),end]},
 {name:"tool scalar arguments",chunks:[begin(),tool(),args("null"),stop(),finish("tool_use"),end]},
 {name:"tool numeric stringification",chunks:[begin(),tool(),args('{"unsafe":9007199254740993,"overflow":1e400,"z":"<>&\\u2028"}'),stop(),finish("tool_use"),end]},
 {name:"interleaved wire indices",chunks:[begin(),start({type:"text"},7),start({type:"thinking"},2),tool({},9),textDelta("answer",7),delta({type:"thinking_delta",thinking:"reason"},2),args('{"a":',9),delta({type:"signature_delta",signature:"sig"},2),args("1}",9),stop(9),stop(7),stop(2),finish("tool_use"),end]},
 {name:"wrong block and unknown delta ignored",chunks:[begin(),text(),args("bad"),delta({type:"thinking_delta",thinking:"bad"}),delta({type:"signature_delta",signature:"bad"}),delta({type:"unknown",text:"bad"}),textDelta("ignored",99),textDelta("ok"),stop(99),stop(),finish(),end]},
 {name:"duplicate index first match",chunks:[begin(),text("one"),text("two"),textDelta("!"),stop(),textDelta("?"),stop(),finish(),end]},
 {name:"missing index first match",chunks:[begin(),{...text("one"),index:undefined},{...textDelta("!"),index:undefined},{...stop(),index:undefined},{...textDelta("?"),index:undefined},finish(),end]},
 {name:"unknown content ignored",chunks:[begin(),start({type:"server_tool_use",id:"server"}),textDelta("ignored"),stop(),finish(),end]},
 {name:"unfinished text accepted",chunks:[begin(),text(),textDelta("partial"),finish(),end]},
 {name:"unfinished tool retains scratch on success",chunks:[begin(),tool(),args('{"partial":'),finish("tool_use"),end]},
 {name:"fallback before content",chunks:[begin(),start({type:"fallback"}),begin({model:"fallback",id:"second"}),text("fallback answer"),stop(),finish(),end]},
 {name:"fallback after output rejected",chunks:[begin(),text("partial"),start({type:"fallback"},1),finish(),end]},
 {name:"fallback with pricing",model:{compat:{allowedFallbackModels:[{provider:"anthropic",model:"fallback",cost:{input:4,output:8,cacheRead:1,cacheWrite:5}}]}},chunks:[begin({model:"fallback"}),finish(),end]},
 {name:"fallback wrong provider no pricing",model:{compat:{allowedFallbackModels:[{provider:"other",model:"fallback",cost:{input:4,output:8,cacheRead:1,cacheWrite:5}}]}},chunks:[begin({model:"fallback"}),finish(),end]},
 {name:"fallback removes requested model tiers",model:{cost:{...model.cost,tiers:[{inputTokensAbove:1,input:999,output:999,cacheRead:999,cacheWrite:999}]},compat:{allowedFallbackModels:[{provider:"anthropic",model:"fallback",cost:{input:4,output:8,cacheRead:1,cacheWrite:5}}]}},chunks:[begin({model:"fallback"}),finish(),end]},
 {name:"fallback uses its own tiers",model:{compat:{allowedFallbackModels:[{provider:"anthropic",model:"fallback",cost:{input:4,output:8,cacheRead:1,cacheWrite:5,tiers:[{inputTokensAbove:1,input:6,output:10,cacheRead:2,cacheWrite:7}]}}]}},chunks:[begin({model:"fallback"}),finish(),end]},
 {name:"response model retained after original model",chunks:[begin({model:"fallback"}),begin(),finish(),end]},
 {name:"response model replaced on another mismatch",chunks:[begin({model:"one"}),begin({model:"two"}),finish(),end]},
 {name:"response model null",chunks:[begin({model:null,id:null}),finish(),end]},
 {name:"usage incremental null preserves",chunks:[begin(),finish("end_turn",{usage:{input_tokens:null,output_tokens:5,cache_read_input_tokens:null,cache_creation_input_tokens:null}}),end]},
 {name:"usage zero resets",chunks:[begin(),finish("end_turn",{usage:{input_tokens:0,output_tokens:0,cache_read_input_tokens:0,cache_creation_input_tokens:0}}),end]},
 {name:"usage thinking subset",chunks:[begin(),finish("end_turn",{usage:{output_tokens:8,output_tokens_details:{thinking_tokens:3}}}),end]},
 {name:"start ignores thinking token detail",chunks:[begin({usage:{output_tokens:8,output_tokens_details:{thinking_tokens:3}}}),finish(),end]},
 {name:"reasoning null preserves then zero",chunks:[begin(),finish(null,{usage:{output_tokens_details:{thinking_tokens:3}}}),finish(null,{usage:{output_tokens_details:{thinking_tokens:null}}}),finish("end_turn",{usage:{output_tokens_details:{thinking_tokens:0}}}),end]},
 {name:"1h cache pricing",chunks:[begin({usage:{input_tokens:20,output_tokens:4,cache_read_input_tokens:2,cache_creation_input_tokens:8,cache_creation:{ephemeral_1h_input_tokens:5}}}),finish(),end]},
 {name:"1h cache delta pricing",chunks:[begin(),finish("end_turn",{usage:{cache_creation_input_tokens:8,cache_creation:{ephemeral_1h_input_tokens:5}}}),end]},
 {name:"1h cache null preserves",chunks:[begin({usage:{cache_creation_input_tokens:8,cache_creation:{ephemeral_1h_input_tokens:5}}}),finish("end_turn",{usage:{cache_creation:{ephemeral_1h_input_tokens:null}}}),end]},
 {name:"1h cache exceeds total",chunks:[begin({usage:{cache_creation_input_tokens:2,cache_creation:{ephemeral_1h_input_tokens:5}}}),finish(),end]},
 {name:"tiered 1h cache",model:{cost:{...model.cost,tiers:[{inputTokensAbove:10,input:4,output:9,cacheRead:1,cacheWrite:6},{inputTokensAbove:10,input:999,output:999,cacheRead:999,cacheWrite:999}]}},chunks:[begin({usage:{input_tokens:20,output_tokens:4,cache_read_input_tokens:2,cache_creation_input_tokens:8,cache_creation:{ephemeral_1h_input_tokens:5}}}),finish(),end]},
 {name:"tier equal threshold",model:{cost:{...model.cost,tiers:[{inputTokensAbove:15,input:4,output:9,cacheRead:1,cacheWrite:6}]}},chunks:[begin(),finish(),end]},
 {name:"managed effort default",model:{compat:{supportsMidConvoEffort:true}},chunks:[begin(),finish(),end]},
 {name:"managed effort explicit",model:{compat:{supportsMidConvoEffort:true}},options:{effort:"max"},chunks:[begin(),finish(),end]},
 {name:"managed effort false",model:{compat:{supportsMidConvoEffort:false}},options:{effort:"low"},chunks:[begin(),finish(),end]},
 ...["end_turn","max_tokens","tool_use","refusal","pause_turn","stop_sequence","sensitive","unknown"].map(reason=>({name:`stop ${reason}`,chunks:[begin(),text("answer"),stop(),finish(reason),end]})),
 {name:"refusal details",chunks:[begin(),finish("refusal",{delta:{stop_reason:"refusal",stop_details:{explanation:"specific reason"}}}),end]},
 {name:"later stop keeps prior error message",chunks:[begin(),finish("refusal"),finish("end_turn"),end]},
 {name:"input transformations diagnostic",chunks:[begin({input_transformations:[{type:"drop",path:"messages.1",reason:"prefix mismatch",extra:"discard"},{type:null,path:null,reason:null},{}]}),finish(),end]},
 {name:"input transformations delta replaces",chunks:[begin({input_transformations:[{type:"old"}]}),finish("end_turn",{input_transformations:[{type:"new",path:[1,2],reason:4}]}),end]},
 {name:"input transformations empty clears",chunks:[begin({input_transformations:[{type:"old"}]}),finish("end_turn",{input_transformations:[]}),end]},
 {name:"input transformations null retains",chunks:[begin({input_transformations:[{type:"old"}]}),finish("end_turn",{input_transformations:null}),end]},
 {name:"no diagnostics on error",chunks:[begin({input_transformations:[{type:"old"}]}),finish("refusal"),end]},
 {name:"null transformation errors at finish",chunks:[begin({input_transformations:[null]}),finish(),end]},
 {name:"oauth restores advertised tool spelling",oauth:true,tools:[declaration("rEaD"),declaration("read")],chunks:[begin(),tool({},0,"Read"),args("{}"),stop(),finish("tool_use"),end]},
 {name:"oauth unknown tool name retained",oauth:true,tools:[declaration("read")],chunks:[begin(),tool({},0,"Other"),args("{}"),stop(),finish("tool_use"),end]},
 {name:"oauth casefold differs from lowercase",oauth:true,tools:[declaration("baſh"),declaration("Skİll")],chunks:[begin(),tool({},0,"Bash"),stop(),tool({},1,"Skill"),stop(1),finish("tool_use"),end]},
 {name:"callback failure strips tool scratch",failBefore:3,chunks:[begin(),tool(),args('{"x":'),finish("tool_use"),end]},
 {name:"callback failure retains usage",failBefore:1,chunks:[begin(),text(),finish(),end]},
 {name:"callback failure before first event",failBefore:0,chunks:[begin(),finish(),end]},
 {name:"abort after text end",abortAtEnd:true,chunks:success},
 {name:"callback changes stop reason",mutate:"stop",chunks:[begin(),finish("end_turn"),end]},
 {name:"callback changes message_stop type cannot defeat framing",mutate:"end",chunks:[begin(),finish(),end]},
 {name:"callback changes message_start type cannot defeat framing",mutate:"start",chunks:[begin(),finish()]},
 {name:"SSE data only ignored",body:`data: ${JSON.stringify(begin())}\n\n`+frame(finish())},
 {name:"SSE unknown named event ignored",body:'event: ping\ndata: {invalid\n\n'+success.map(frame).join("")},
 {name:"SSE named error is raw payload",body:frame(begin())+'event: error\ndata: {"error":{"message":"overloaded"}}\n\n'},
 {name:"SSE unnamed DONE ignored",body:success.map(frame).join("")+'data: [DONE]\n\n'},
 {name:"SSE EOF trailing event",body:success.slice(0,-1).map(frame).join("")+`event: message_stop\ndata: ${JSON.stringify(end)}`},
 {name:"SSE multiline CR",body:frame(begin()).replaceAll("\n","\r")+'event: message_delta\rdata: {"type":"message_delta",\rdata: "delta":{"stop_reason":"end_turn"}}\r\r'+frame(end).replaceAll("\n","\r")},
 {name:"SSE first BOM only",body:'\ufeff'+success.map(frame).join("")},
 {name:"SSE later BOM not stripped",body:frame(begin())+'\ufeff'+frame(finish())+frame(end)},
 {name:"SSE JSON escape repair",body:frame(begin())+frame(text())+'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"C:\\q"}}\n\n'+frame(stop())+frame(finish())+frame(end)},
 {name:"SSE null event error context",body:'event: message_start\ndata: null\n\n'},
 {name:"SSE byte split Unicode",partSizes:[1],chunks:[begin(),text(),textDelta("😀 tiếng Việt"),stop(),finish(),end]},
 {name:"SSE split CRLF source framing",partSizes:[1],body:success.map(frame).join("").replaceAll("\n","\r\n")},
 {name:"SSE completed before start still counts end",chunks:[end,begin(),finish()]},
];
const originalPush=AssistantMessageEventStream.prototype.push;const cases=[];
for(const input of inputs){
 const chosen={...model,...input.model};const events:any[]=[];const live:any[]=[];const controller=new AbortController();
 AssistantMessageEventStream.prototype.push=function(event:any){events.push(JSON.parse(JSON.stringify(event)));live.push(event);if(input.abortAtEnd&&event.type==="text_end")controller.abort();return originalPush.call(this,event)};
 const body=input.body??input.chunks.map(frame).join("");const bytes=new TextEncoder().encode(body);
 const parts:Uint8Array[]=[];let offset=0,index=0;while(offset<bytes.length){const size=input.partSizes?.[index++%input.partSizes.length]??bytes.length;parts.push(bytes.slice(offset,offset+size));offset+=size}
 let received=0;
 const resultStream=stream(chosen,normalizeContext({messages:[],tools:input.tools??[]}),{...input.options,apiKey:input.oauth?"sk-ant-oat01-fixture":"sk-fixture",signal:controller.signal,
 fetch:async()=>new Response(new ReadableStream({start(c){for(const part of parts)c.enqueue(part);c.close()}}),{headers:{"content-type":"text/event-stream"}}),
 onProviderStreamEvent:(event:any)=>{if(received++===input.failBefore)throw new Error("fixture failure");if(input.mutate==="stop"&&event.type==="message_delta")event.delta.stop_reason="max_tokens";if(input.mutate==="end"&&event.type==="message_stop")event.type="ignored";if(input.mutate==="start"&&event.type==="message_start")event.type="ignored"},
 });
 const result=await resultStream.result();for await(const _ of resultStream){}
 for(const event of live)if(event.partial&&event.partial!==result)throw new Error("source lost partial identity");
 cases.push({input,expected:{events,result}});
}
AssistantMessageEventStream.prototype.push=originalPush;
const file=new URL("pi-anthropic-stream.json",import.meta.url);const output=JSON.stringify({upstreamCommit:manifest.commit,model,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic stream oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic stream/SSE cases against Pi ${manifest.commit}`);
