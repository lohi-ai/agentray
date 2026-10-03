import { readFileSync, writeFileSync } from "node:fs";
import { PiWorker, manifest, AssistantMessageEventStream } from "./worker-oracle.ts";
(globalThis as any).PI_UPSTREAM_COMMIT=manifest.commit;
Date.now=()=>100;
const model={id:"test",api:"test",provider:"test"};
const tool={name:"echo",label:"Echo",description:"Echo",parameters:{type:"object",properties:{value:{type:"string"}},required:["value"]}};
const message=(content:any,stopReason="stop")=>({role:"assistant",content,stopReason,timestamp:100,api:"test",provider:"test",model:"test",usage:{input:1,output:1,cacheRead:0,cacheWrite:0,totalTokens:2,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}}});
const reply=message([{type:"text",text:"done"}]);
const calls=message([{type:"toolCall",id:"c1",name:"echo",arguments:{value:"hello"}}],"toolUse");
const result={content:[{type:"text",text:"hello"}],details:{retained:true}};
const user=(text:string)=>({role:"user",content:text,timestamp:100});
const prompt={method:"prompt",params:{input:"test"}};
const inputs:any[]=[
 {name:"defaults",actions:[]},
 {name:"native provider trace",options:{traceRequests:true,streamMode:"native"},actions:[prompt],responses:[reply]},
 {name:"native provider error trace",options:{traceRequests:true,streamMode:"native"},actions:[prompt],responses:[{...reply,stopReason:"error",errorMessage:"failed"}]},
 {name:"native provider throws",options:{traceRequests:true,streamMode:"native"},actions:[prompt],streamFailure:"provider failed"},
 {name:"trace plain prompt",options:{traceRequests:true},actions:[prompt],responses:[reply]},
 {name:"trace excludes credentials",options:{traceRequests:true,callbacks:["getApiKey"],streamOptions:{maxTokens:12}},actions:[prompt],responses:[reply],hookResults:{getApiKey:["private-key"]}},
 {name:"trace tool round trip",options:{traceRequests:true,initialState:{model,tools:[tool]}},actions:[prompt],responses:[calls,reply],toolResult:result},
 {name:"trace provider failure",options:{traceRequests:true},actions:[prompt],streamFailure:"provider failed"},
 {name:"trace returned error",options:{traceRequests:true},actions:[prompt],responses:[{...reply,stopReason:"error",errorMessage:"failed"}]},
 {name:"trace across runs",options:{traceRequests:true},actions:[prompt,prompt],responses:[reply,reply]},
 {name:"plain prompt",actions:[prompt],responses:[reply]},
 {name:"image prompt",actions:[{method:"prompt",params:{input:"",images:[{type:"image",data:"YQ==",mimeType:"image/png"}]}}],responses:[reply]},
 {name:"tool round trip",options:{initialState:{model,tools:[tool]}},actions:[prompt],responses:[calls,reply],toolResult:result,toolUpdates:[{content:[{type:"text",text:"working"}],details:{progress:1}}]},
 {name:"hooks and key",options:{initialState:{model,tools:[tool]},callbacks:["getApiKey","beforeToolCall","afterToolCall","finishTurn"]},actions:[prompt],responses:[calls,reply],toolResult:result,hookResults:{getApiKey:["key-one","key-two"],beforeToolCall:[null],afterToolCall:[{content:[{type:"text",text:"overridden"}],isError:false}],finishTurn:[null,{action:"end"}]}},
 {name:"blocked tool",options:{initialState:{tools:[tool]},callbacks:["beforeToolCall"]},actions:[prompt],responses:[calls,reply],hookResults:{beforeToolCall:[{block:true,reason:"denied"}]}},
 {name:"tool error",options:{initialState:{tools:[tool]}},actions:[prompt],responses:[calls,reply],toolFailure:"tool failed"},
 {name:"tool returned error",options:{initialState:{tools:[tool]}},actions:[prompt],responses:[calls,reply],toolResult:{...result,isError:true}},
 {name:"tool terminates",options:{initialState:{tools:[tool]}},actions:[prompt],responses:[calls],toolResult:{...result,terminate:true}},
 {name:"finish continues",options:{callbacks:["finishTurn"]},actions:[prompt],responses:[reply,reply],hookResults:{finishTurn:[{action:"continue"},{action:"end"}]}},
 {name:"legacy next preparation",options:{callbacks:["finishTurn","prepareNextTurn"]},actions:[prompt],responses:[reply,reply],hookResults:{finishTurn:[{action:"continue"},{action:"end"}],prepareNextTurn:[{messages:[user("prepared")],thinkingLevel:"high"}]}},
 {name:"context next preparation wins",options:{callbacks:["finishTurn","prepareNextTurn","prepareNextTurnWithContext"]},actions:[prompt],responses:[reply,reply],hookResults:{finishTurn:[{action:"continue"},{action:"end"}],prepareNextTurnWithContext:[{messages:[user("prepared")]}]}},
 {name:"request replacement binds new tools",options:{callbacks:["prepareRequest"]},actions:[prompt],responses:[calls,reply],toolResult:result,hookResults:{prepareRequest:[{context:{messages:[user("replacement")],tools:[tool]},model:{...model,id:"temporary"},thinkingLevel:"high"},null]}},
 {name:"transform then convert",options:{callbacks:["transformContext","convertToLlm"]},actions:[prompt],responses:[reply],hookResults:{transformContext:[[user("transformed")]],convertToLlm:[[user("converted")]]}},
 {name:"settings retain callbacks",options:{callbacks:["getApiKey"],streamOptions:{maxTokens:12,temperature:0}},actions:[{method:"configure",params:{sessionId:"sid",thinkingBudgets:{high:42},transport:"sse",maxRetryDelayMs:3,steeringMode:"all"}},{method:"setState",params:{thinkingLevel:"high"}},prompt],responses:[reply],hookResults:{getApiKey:["key"]}},
 {name:"state replacement binds tools",actions:[{method:"setState",params:{model,tools:[tool],messages:[user("prior")],thinkingLevel:"low"}},{method:"continue"}],responses:[calls,reply],toolResult:result},
 {name:"queues and reset",actions:[{method:"steer",params:user("steer")},{method:"followUp",params:user("follow")},{method:"hasQueuedMessages"},{method:"peekQueuedMessages"},{method:"clearAllQueues"},{method:"reset"}]},
 {name:"persistent request ids",actions:[prompt,prompt],responses:[reply,reply]},
 {name:"callback failure",actions:[prompt],streamFailure:"provider failed"},
 {name:"hook failure",options:{callbacks:["prepareRequest"]},actions:[prompt],hookFailures:{prepareRequest:"prepare failed"}},
 {name:"ask preparation",options:{initialState:{tools:[{...tool,name:"ask",parameters:{type:"object",properties:{question:{type:"string"}},required:["question"]},agentrayPrepareArguments:"ask-v1"}]}},actions:[prompt],responses:[message([{type:"toolCall",id:"ask-1",name:"ask",arguments:{question:"  question  "}}],"toolUse"),reply],toolResult:result},
 {name:"invalid setting",actions:[{method:"configure",params:{invalid:true}}]},
 {name:"read-only state",actions:[{method:"setState",params:{isStreaming:true}}]},
 {name:"invalid callback",options:{callbacks:["missing"]},actions:[]},
 {name:"forbidden stream option",options:{streamOptions:{apiKey:"not allowed"}},actions:[]},
];
const clone=(value:any)=>value===undefined?null:JSON.parse(JSON.stringify(value));
const cases=[];
for(const input of inputs){
 let id=0,responseIndex=0;
 const pending=new Map<string,{resolve:(value:any)=>void,reject:(error:Error)=>void}>();
 const indices=new Map<string,number>();
 const callbacks:any[]=[],events:any[]=[],actions:any[]=[],traces:any[]=[];
 (globalThis as any).nativeOracleStream=(model:any,context:any,options:any)=>{
  // Go carries AbortSignal as context.Context, outside serializable options.
  const {telemetryContext,signal,...serializable}=options;
  callbacks.push({method:"nativeStream",params:clone({model,context,options:serializable})});
  if(input.streamFailure)throw new Error(input.streamFailure);
  telemetryContext.startSpan({name:"provider.request",attributes:{transport:"test"}},()=>undefined);
  const stream=new AssistantMessageEventStream();
  const response=clone(input.responses[responseIndex++]);
  stream.push({type:"start",partial:response});
  if(response.stopReason==="error"||response.stopReason==="aborted")stream.push({type:"error",reason:response.stopReason,error:response});
  else stream.push({type:"done",reason:response.stopReason,message:response});
  stream.end();return stream;
 };
 const worker=new PiWorker((frame:any)=>{
  if(frame.kind==="result"){
   const waiting=pending.get(frame.id)!;pending.delete(frame.id);
   if(frame.error)waiting.reject(Object.assign(new Error(frame.error.message),{name:frame.error.name}));else waiting.resolve(frame.value);
  }else if(frame.kind==="callback"){
   queueMicrotask(()=>{
    const params=clone(frame.params);
    if(frame.method==="event")events.push(params);else if(frame.method==="trace")traces.push(params);else callbacks.push({method:frame.method,params});
    let value:any,failure:string|undefined;
    if(frame.method==="stream"){failure=input.streamFailure;value=input.responses?.[responseIndex++];if(!failure&&!value)failure="script exhausted"}
    else if(frame.method==="tool"){
     for(const update of input.toolUpdates??[])worker.receive({kind:"progress",id:frame.id,value:clone(update)});
     failure=input.toolFailure;value=input.toolResult;
    }else if(frame.method!=="event"&&frame.method!=="trace"){
     const index=indices.get(frame.method)??0;indices.set(frame.method,index+1);
     failure=input.hookFailures?.[frame.method];value=input.hookResults?.[frame.method]?.[index];
    }
    worker.receive({kind:"result",id:frame.id,...(failure?{error:{name:"Error",message:failure}}:{value:clone(value)})});
   });
  }
 });
 const call=(method:string,params?:any)=>new Promise<any>((resolve,reject)=>{const key=String(++id);pending.set(key,{resolve,reject});worker.receive({kind:"call",id:key,method,params:clone(params)})});
 let initError:string|null=null,state:any=null,spans:any=[];
 try{await call("initialize",input.options??{})}catch(error){initError=(error as Error).message}
 if(!initError){
  for(const action of input.actions){
   let value:any=null,error:string|null=null;
   try{value=clone(await call(action.method,action.params))}catch(failure){error=(failure as Error).message}
   actions.push({method:action.method,value,error,state:clone(await call("state"))});
  }
  state=clone(await call("state"));spans=clone(await call("telemetry"));
 }
 cases.push({input,expected:{initError,callbacks,events,actions,state,spans,traces}});
 worker.close();
}
const output=JSON.stringify({upstreamCommit:manifest.commit,now:100,cases},null,2)+"\n";
const destination=new URL("native-agent.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Native agent fixtures changed; regenerate and review")}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} native agent host cases against the worker and Pi ${manifest.commit}`);
