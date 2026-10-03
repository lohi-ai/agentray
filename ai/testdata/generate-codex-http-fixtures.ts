import {readFileSync,writeFileSync} from "node:fs";
import {zstdDecompressSync} from "node:zlib";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {stream}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
Date.now=()=>100;
const shared=JSON.parse(readFileSync(new URL('pi-codex-params.json',import.meta.url),'utf8'));
const model={...shared.model,baseUrl:'https://codex.example/backend'};const context=shared.context;
const token='header.'+Buffer.from(JSON.stringify({'https://api.openai.com/auth':{chatgpt_account_id:'account'}})).toString('base64')+'.signature';
const end={type:'response.done',response:{id:'response',status:'completed',output:[],usage:{input_tokens:3,output_tokens:2,total_tokens:5}}};
const inputs:any[]=[
 {name:'default'},
 {name:'session',options:{sessionId:'session'}},
 {name:'cache none',options:{sessionId:'session',cacheRetention:'none'}},
 {name:'empty session',options:{sessionId:''}},
 {name:'cache clipped',options:{sessionId:'s'.repeat(70)}},
 {name:'header overrides',headers:{'x-model':'initial'},options:{headers:{'x-model':null,'x-request':'new',authorization:'wrong'}}},
 {name:'payload replacement',payload:{model:'replaced',stream:true,input:[],marker:'replacement'}},
 {name:'payload failure',payloadFailure:true},
 {name:'response failure',responseFailure:true},
 {name:'event failure',eventFailure:true,options:{maxRetries:2}},
 {name:'no response body',responses:[{status:204}]},
 {name:'non-JSON billing catch retry',options:{maxRetries:1},responses:[{status:429,body:'billing'},{}]},
 {name:'event mutation',eventMutation:true},
 {name:'missing key',options:{apiKey:''}},
 {name:'invalid token',options:{apiKey:'invalid'}},
 {name:'negative timeout',options:{timeoutMs:-1}},
 {name:'null timeout',options:{timeoutMs:null}},
 {name:'fractional timeout',options:{timeoutMs:10.5}},
 {name:'transient retry',options:{maxRetries:1},responses:[{status:503,body:'overloaded',headers:{'retry-after-ms':'0'}},{}]},
 {name:'retry delay ceiling',options:{maxRetries:1,maxRetryDelayMs:1000},responses:[{status:503,body:'overloaded',headers:{'retry-after-ms':'2001'}}]},
 {name:'quota no retry',options:{maxRetries:2},responses:[{status:429,body:JSON.stringify({error:{code:'usage_limit_reached',plan_type:'PLUS',resets_at:60}})}]},
 {name:'unauthorized retries through catch',options:{maxRetries:1},responses:[{status:401,body:JSON.stringify({error:{message:'unauthorized'}})},{}]},
 {name:'max retries negative',options:{maxRetries:-1}},
 {name:'server body error',responses:[{status:400,body:'invalid request'}]},
];
const cases=[];
for(const input of inputs){
 const requests:any[]=[],callbacks:string[]=[];let attempt=0;
 const chosen={...model,headers:input.headers};
 const s=stream(chosen,normalizeContext(context),{apiKey:token,transport:'sse',...input.options,
  onPayload:()=>{callbacks.push('payload');if(input.payloadFailure)throw new Error('payload failure');return input.payload},
  onResponse:()=>{callbacks.push('response');if(input.responseFailure)throw new Error('response failure')},
  onProviderStreamEvent:(event:any)=>{callbacks.push('event');if(input.eventFailure)throw new Error('event failure');if(input.eventMutation)event.response.id='mutated'},
  fetch:async(url:any,init:any)=>{
   const headers=Object.fromEntries(new Headers(init.headers).entries());headers['user-agent']='<platform>';
   const body=typeof init.body==='string'?init.body:zstdDecompressSync(init.body).toString();
   requests.push({url:String(url),headers,body:JSON.parse(body)});
   const spec=input.responses?.[attempt++]??{};
   return new Response(spec.status===204?null:(spec.body??`data: ${JSON.stringify(end)}\n\n`),{status:spec.status??200,headers:{'content-type':'text/event-stream','x-fixture':'yes',...spec.headers}});
  }
 });
 const result=await s.result();for await(const _ of s){}
 cases.push({input,expected:{requests,callbacks,result}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,model,context,token,cases},null,2)+'\n';const file=new URL('pi-codex-http.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==output)throw new Error('Codex HTTP oracle changed')}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex HTTP cases against Pi ${manifest.commit}`);
