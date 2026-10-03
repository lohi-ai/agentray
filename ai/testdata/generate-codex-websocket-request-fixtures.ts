import {readFileSync,writeFileSync} from 'node:fs';
import {plugin} from 'bun';
import {aiRoot,manifest} from '../../agentcore/engine/testdata/oracle.ts';
plugin({name:'codex-ws-request-oracle',setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:'ts',contents:readFileSync(args.path,'utf8')+'\nexport {processWebSocketStream as processStream,websocketSessionCache as cache};\n'}))}});
let chunks:any[]=[];let requests:any[]=[];let closes:any[]=[];
class FakeSocket extends EventTarget{
 readyState=1;
 constructor(...args:any[]){super();queueMicrotask(()=>this.dispatchEvent(new Event('open')))}
 send(payload:string){requests.push(JSON.parse(payload));void(async()=>{for(const chunk of chunks){await Promise.resolve();this.dispatchEvent(new MessageEvent('message',{data:JSON.stringify(chunk)}))}})()}
 close(code:number,reason:string){closes.push({code,reason});this.readyState=3}
}
globalThis.WebSocket=FakeSocket as any;
const {processStream,cache,getOpenAICodexWebSocketDebugStats:getStats,resetOpenAICodexWebSocketDebugStats:reset,closeOpenAICodexWebSocketSessions}=await import(new URL('api/openai-codex-responses.ts',aiRoot).pathname);
const source=JSON.parse(readFileSync(new URL('pi-codex-stream.json',import.meta.url),'utf8'));
const inputs=source.cases.filter(c=>!c.input.failBefore&&!c.input.abortAtEnd&&c.input.chunks.some(x=>['response.done','response.completed','response.incomplete','error','response.failed'].includes(x.type))).map(c=>({...c.input,transport:'websocket-cached'}));
inputs.push({name:'callback failure before start',chunks:[{type:'response.done',response:{id:'r',status:'completed',output:[]}}],callbackFail:true,transport:'websocket-cached'});
inputs.push({name:'stats explicit websocket store true',transport:'websocket',body:{model:'test',store:true,input:[{role:'user',content:'x'}]},chunks:[{type:'response.done',response:{id:'r',status:'completed',output:[]}}]});
inputs.push({name:'stats supplied previous response',transport:'auto',body:{model:'test',store:false,previous_response_id:'prior',input:[]},chunks:[{type:'response.done',response:{id:'r',status:'completed',output:[]}}]});
const cases=[];
for(const input of inputs){cache.clear();reset();requests=[];closes=[];chunks=input.chunks;
 const model={...source.model,...input.model,...(input.compat?{compat:input.compat}:{})};
 const output={role:'assistant',content:[],api:'openai-codex-responses',provider:model.provider,model:model.id,usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:'pending',timestamp:123};
 const events:any[]=[];const stream={push:(e:any)=>events.push(structuredClone(e))};let error;
 const body=input.body??{model:model.id,store:false,input:[]};const options={...input.options,transport:input.transport,onProviderStreamEvent:input.callbackFail?()=>{throw Error('callback failed')}:undefined};
 try{await processStream('wss://example.test',body,new Headers(),output,stream,model,()=>stream.push({type:'start',partial:output}),undefined,undefined,'s','a',new Map(),options)}catch(e){error={message:e.message,name:e.name}}
 const entry=cache.get('s')?.get('a');const continuation=entry?.continuation;
 const expected={stats:getStats('s'),requests:structuredClone(requests),events,output,...(error?{error}:{}),closes:structuredClone(closes),cached:!!entry,...(continuation?{continuation}:{} )};
 closeOpenAICodexWebSocketSessions();cases.push({input,expected});
}
const out=JSON.stringify({upstreamCommit:manifest.commit,model:source.model,cases},null,2)+'\n';const file=new URL('pi-codex-websocket-request.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==out)throw Error('WebSocket request oracle changed')}else writeFileSync(file,out);
console.log(`Verified ${cases.length} Codex WebSocket request cases`);
