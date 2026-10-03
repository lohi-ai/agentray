import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"codex-errors-oracle",setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {isRetryableError as retryable,getRetryAfterDelayMs as retryAfter,validateRetryDelayMs as validateDelay,parseErrorResponse as parseError};\n"}))}});
const {retryable,retryAfter,validateDelay,parseError}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const now=1700000000000;Date.now=()=>now;
const retry=[400,401,403,429,500,501,502,503,504].flatMap(status=>["ordinary error","overloaded","billing","rate limit","connection refused"].map(text=>({status,text,expected:retryable(status,text)})));
for(const text of ["GoUsageLimitError","FreeUsageLimitError","Monthly usage limit reached","available balance","insufficient_quota","out of budget","quota exceeded","BILLING","service-unavailable","upstream_connect","rate\nlimit"])retry.push({status:429,text,expected:retryable(429,text)});
const delays=[{},...['','0','-1','0.25','0x10','Infinity','invalid'].map(value=>({'retry-after-ms':value,'retry-after':'2'})),...['','0','-1','1.5','0x10','Wed, 15 Nov 2023 00:13:20 GMT','Tue, 14 Nov 2023 20:13:20 GMT','invalid'].map(value=>({'retry-after':value}))].map(headers=>({headers,expected:retryAfter(new Headers(headers))??null}));
const limits=[undefined,null,0,-1,1000,1001,60000].flatMap(limit=>[1000,1001,61001].map(delay=>{let error=null;try{validateDelay(delay,{maxRetryDelayMs:limit})}catch(e){error=(e as Error).message}return{delay,limit,error}}));
const inputs:any[]=[
 {raw:'',status:500,statusText:''}, {raw:'',status:500,statusText:'Server Error'}, {raw:'unparsed',status:502},
 ...[{}, {message:'raw error'}, {code:'usage_limit_reached',message:'raw quota',plan_type:'PLUS',resets_at:now/1000+90}, {type:'usage_not_included',plan_type:'PRO',resets_at:now/1000+29}, {code:'rate_limit_exceeded',resets_at:now/1000-30}, {code:'other',plan_type:'TEAM'}, {code:'usage_limit_reached',plan_type:42}, {code:'usage_limit_reached',resets_at:'invalid'}].flatMap(error=>[400,429].map(status=>({raw:JSON.stringify({error}),status}))),
];
const errors=[];for(const input of inputs)errors.push({input,expected:await parseError(new Response(input.raw,{status:input.status,statusText:input.statusText??''}))});
const output=JSON.stringify({upstreamCommit:manifest.commit,now,retry,delays,limits,errors},null,2)+"\n";const file=new URL('pi-codex-errors.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==output)throw new Error('Codex error oracle changed')}else writeFileSync(file,output);
console.log(`Verified ${retry.length+delays.length+limits.length+errors.length} Codex error/retry cases against Pi ${manifest.commit}`);
