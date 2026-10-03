import {readFileSync,writeFileSync} from 'node:fs';
import {plugin} from 'bun';
import {aiRoot,manifest} from '../../agentcore/engine/testdata/oracle.ts';
plugin({name:'codex-ws-stream-oracle',setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:'ts',contents:readFileSync(args.path,'utf8')+'\nexport {parseWebSocket as parse};\n'}))}});
const {parse}=await import(new URL('api/openai-codex-responses.ts',aiRoot).pathname);
const message=(data:any,binary=false)=>({kind:'message',data:JSON.stringify(data),binary});
const close=(data:any={})=>({kind:'close',data});
const inputs:any[]=[
 ...['response.completed','response.done','response.incomplete'].map(type=>({name:type,steps:[message({type:'response.created'}),message({type}),close({code:1006})]})),
 {name:'early close',steps:[message({type:'response.created'}),close({code:1006,reason:'lost',wasClean:false})]},
 ...[{},null,{code:1009},{code:1009,reason:'large',wasClean:true},{code:'bad',reason:4,wasClean:'yes'},{code:1000,reason:'  '}].map((data,i)=>({name:`close metadata ${i}`,steps:[close(data)]})),
 ...[{},null,{message:'outer',error:{message:'inner'}},{message:'',error:{message:'inner'}},{error:{message:4}}].map((data,i)=>({name:`error ${i}`,steps:[{kind:'error',data},close({code:1006})]})),
 {name:'binary unicode',steps:[message({type:'delta',text:'Chào 🌱'},true),message({type:'response.done'},true)]},
 {name:'binary BOM',steps:[{kind:'message',data:'\ufeff{"type":"response.done"}',binary:true}]},
 {name:'empty ignored',steps:[{kind:'message',data:''},message({type:'response.done'})]},
 {name:'primitive permitted',steps:[message(1),message('x'),message([]),message({type:'response.done'})]},
 {name:'failed event is not terminal',steps:[message({type:'response.failed'}),close({code:1000})]},
 {name:'aborted',steps:[{kind:'abort'}]},
 {name:'idle timeout',steps:[],idle:1},
];
const cases=[];
for(const input of inputs){const listeners=new Map();const controller=new AbortController();const closes:any[]=[];
 const socket={addEventListener:(k,f)=>listeners.set(k,f),removeEventListener:(k,f)=>listeners.delete(k),close:(code,reason)=>closes.push({code,reason})};
 const events:any[]=[];let error:any;const task=(async()=>{try{for await(const event of parse(socket,controller.signal,input.idle))events.push(event)}catch(e){error={message:e.message,...(e.code!==undefined?{code:e.code}:{}),...(e.reason!==undefined?{reason:e.reason}:{}),...(e.wasClean!==undefined?{wasClean:e.wasClean}:{})}}})();
 for(const step of input.steps){if(step.kind==='abort')controller.abort();else {const data=step.kind==='message'?{data:step.binary?new TextEncoder().encode(step.data):step.data}:step.data;listeners.get(step.kind)?.(data)};for(let i=0;i<5;i++)await Promise.resolve()}
 await task;cases.push({input,expected:{events,...(error?{error}:{}),closes,listeners:listeners.size}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+'\n';const file=new URL('pi-codex-websocket-stream.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==output)throw Error('WebSocket stream oracle changed')}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex WebSocket stream cases`);
