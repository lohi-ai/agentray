import {readFileSync,writeFileSync} from 'node:fs';
import {plugin} from 'bun';
import {aiRoot,manifest} from '../../agentcore/engine/testdata/oracle.ts';
plugin({name:'codex-cache-oracle',setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:'ts',contents:readFileSync(args.path,'utf8')+'\nexport {acquireWebSocket as acquire,websocketSessionCache as cache};\n'}))}});
let sockets:any[]=[];let closes:any[]=[];
class FakeSocket extends EventTarget {
 id:number;readyState:any=1;
 constructor(...args:any[]){super();this.id=sockets.length;sockets.push(this);queueMicrotask(()=>this.dispatchEvent(new Event('open')))}
 close(code:number,reason:string){closes.push({socket:this.id,code,reason});this.readyState=3}
}
globalThis.WebSocket=FakeSocket as any;
const {acquire,cache,closeOpenAICodexWebSocketSessions:closeSessions}=await import(new URL('api/openai-codex-responses.ts',aiRoot).pathname);
let now=0;let sequence=0;const timers=new Map<number,{due:number,run:()=>void}>();
Date.now=()=>now;
globalThis.setTimeout=((run:any,delay:number)=>{const id=++sequence;timers.set(id,{due:now+delay,run});return id}) as any;
globalThis.clearTimeout=((id:number)=>timers.delete(id)) as any;
const a=(lease:string,session='s',account='a')=>({op:'acquire',lease,session,account});
const r=(lease:string,keep=true)=>({op:'release',lease,keep});
const tick=(ms:number,fire=true)=>({op:'time',ms,fire});
const inputs=[
 {name:'ephemeral',actions:[a('x',''),r('x')]},
 {name:'reuse',actions:[a('x'),r('x'),a('y'),r('y',false)]},
 {name:'busy sibling',actions:[a('x'),a('y'),r('y'),r('x'),a('z')]},
 {name:'account isolation',actions:[a('x'),a('y','s','b'),r('x'),a('z')]},
 {name:'session isolation',actions:[a('x'),a('y','t'),{op:'close',session:'s'}]},
 {name:'idle expiry',actions:[a('x'),r('x'),tick(299999),tick(1),a('y')]},
 {name:'cancel idle on reuse',actions:[a('x'),r('x'),tick(299999),a('y'),tick(1),r('y'),tick(300000)]},
 {name:'age expiry',actions:[a('x'),r('x'),tick(3300000,false),a('y')]},
 {name:'busy age retained',actions:[a('x'),tick(3300000),a('y'),r('y')]},
 {name:'closed socket',actions:[a('x'),r('x'),{op:'state',lease:'x',state:3},a('y')]},
 {name:'unknown state',actions:[a('x'),r('x'),{op:'state',lease:'x'},a('y')]},
 {name:'closed at release',actions:[a('x'),{op:'state',lease:'x',state:3},r('x')]},
 {name:'stale release preserves replacement',actions:[a('x'),{op:'close',session:'s'},a('y'),r('x',false)]},
 {name:'nested insertion order',actions:[a('x'),a('y','t'),a('z','s','b'),{op:'close',session:''}]},
 {name:'close all',actions:[a('x'),a('y','t'),r('x'),r('y'),{op:'close',session:''},tick(300000)]},
 {name:'repeated reused release',actions:[a('x'),r('x'),a('y'),r('y'),r('y',false),tick(300000)]},
];
const cases=[];
for(const input of inputs){cache.clear();timers.clear();now=0;sockets=[];closes=[];const leases=new Map();const expected=[];
 for(const step of input.actions as any[]){
  if(step.op==='acquire')leases.set(step.lease,await acquire('wss://example.test',new Headers(),step.session,step.account));
  if(step.op==='release')leases.get(step.lease).release({keep:step.keep});
  if(step.op==='state')leases.get(step.lease).socket.readyState=step.state;
  if(step.op==='close')closeSessions(step.session);
  if(step.op==='time'){now+=step.ms;if(step.fire)for(const [id,timer] of [...timers])if(timer.due<=now){timers.delete(id);timer.run()}}
  expected.push({leases:[...leases].map(([name,l])=>({name,socket:l.socket.id,reused:l.reused,cached:!!l.entry})),entries:[...cache].flatMap(([session,accounts])=>[...accounts].map(([account,e])=>({session,account,socket:e.socket.id,busy:e.busy}))).sort((a,b)=>(a.session+a.account).localeCompare(b.session+b.account)),closes:structuredClone(closes)});
 }
 cases.push({input,expected});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+'\n';const file=new URL('pi-codex-socket-cache.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==output)throw Error('Cache oracle changed')}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex socket cache cases`);
