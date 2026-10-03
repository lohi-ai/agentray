import {readFileSync,writeFileSync} from 'node:fs';
import {aiRoot,manifest} from '../../agentcore/engine/testdata/oracle.ts';
let scenarios:string[]=[];let attempts=0;let active:any;let abort:AbortController;
const end={type:'response.done',response:{id:'r',status:'completed',output:[]}};
class FakeSocket extends EventTarget{
 readyState=1;
 constructor(...args:any[]){super();queueMicrotask(()=>this.dispatchEvent(new Event('open')))}
 send(payload:string){const scenario=scenarios[attempts++]??'success';active=scenario;void(async()=>{
  const message=(data:any)=>this.dispatchEvent(new MessageEvent('message',{data:JSON.stringify(data)}));
  const wait=async()=>{for(let i=0;i<12;i++)await Promise.resolve()};await wait();
  if(scenario.startsWith('after-')){message({type:'response.created',response:{id:'r'}});await wait()}
  if(scenario==='abort'){abort.abort();return}
  if(scenario.endsWith('network')){const e=new Event('error');Object.assign(e,{message:'network failed'});this.dispatchEvent(e);return}
  const code=scenario.endsWith('limit')?'websocket_connection_limit_reached':scenario.endsWith('missing')?'previous_response_not_found':scenario==='api'?'invalid_request_error':null;
  if(code){message({type:'error',code,message:code});return}
  message(end);
 })()}
 close(){this.readyState=3}
}
globalThis.WebSocket=FakeSocket as any;
const {stream,getOpenAICodexWebSocketDebugStats:getStats,resetOpenAICodexWebSocketDebugStats:reset,closeOpenAICodexWebSocketSessions:close}=await import(new URL('api/openai-codex-responses.ts',aiRoot).pathname);
const {normalizeContext}=await import(new URL('utils/transcript.ts',aiRoot).pathname);
const token='h.'+Buffer.from(JSON.stringify({'https://api.openai.com/auth':{chatgpt_account_id:'a'}})).toString('base64')+'.s';
const model={id:'test',name:'Test',api:'openai-codex-responses',provider:'openai-codex',baseUrl:'https://example.test',reasoning:false,input:['text'],maxTokens:100,contextWindow:1000,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
const inputs:any[]=[
 ...[['success'],['network'],['after-network'],['limit','success'],['limit','limit'],['missing','success'],['missing','missing'],['limit','missing','success'],['missing','limit','network'],['after-missing','success'],['after-limit'],['api'],['callback'],['abort']].map(scenarios=>({name:scenarios.join(' then '),scenarios})),
 {name:'explicit SSE',transport:'sse',scenarios:['network']},
 {name:'explicit WS still falls back',transport:'websocket',scenarios:['network']},
 {name:'session fallback retained',scenarios:['network','success'],calls:2},
 {name:'no session no sticky fallback',session:'',scenarios:['network','success'],calls:2},
 {name:'reset clears fallback',scenarios:['network','success'],calls:2,reset:true},
];
const cases=[];
for(const input of inputs){close();reset();attempts=0;scenarios=input.scenarios;let sse=0;const results=[];
 for(let call=0;call<(input.calls??1);call++){if(input.reset&&call)reset('s');abort=new AbortController();const before=attempts;let starts=0;
 const output=stream(model,normalizeContext({messages:[]}),{apiKey:token,sessionId:input.session??'s',transport:input.transport??'auto',signal:abort.signal,onProviderStreamEvent:()=>{if(active==='callback')throw Error('callback failed')},fetch:async()=>{sse++;return new Response('data: '+JSON.stringify(end)+'\n\n',{status:200,headers:{'content-type':'text/event-stream'}})}});
 for await(const event of output){if(event.type==='start')starts++}
 const result=await output.result();results.push({attempts:attempts-before,sse,starts,stopReason:result.stopReason,...(result.errorMessage?{error:result.errorMessage}:{}),...(getStats(input.session??'s')?{stats:getStats(input.session??'s')} : {}),diagnostics:(result.diagnostics??[]).map(d=>({type:d.type,eventsEmitted:d.details?.eventsEmitted,phase:d.details?.phase,fallbackTransport:d.details?.fallbackTransport}))});
 }
 cases.push({input,expected:results});
}
close();reset();const out=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+'\n';const file=new URL('pi-codex-transport-policy.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==out)throw Error('Transport policy oracle changed')}else writeFileSync(file,out);
console.log(`Verified ${cases.length} Codex transport policy cases`);
