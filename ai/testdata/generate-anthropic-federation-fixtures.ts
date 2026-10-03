import {readFileSync,writeFileSync,mkdtempSync,rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {streamSimple}=await import(new URL("api/anthropic-messages.ts",aiRoot).pathname);
const {normalizeContext}=await import(new URL("utils/transcript.ts",aiRoot).pathname);
for(const key of ["ANTHROPIC_CUSTOM_HEADERS","ANTHROPIC_FEDERATION_RULE_ID","ANTHROPIC_ORGANIZATION_ID","ANTHROPIC_IDENTITY_TOKEN_FILE","ANTHROPIC_SERVICE_ACCOUNT_ID","ANTHROPIC_WORKSPACE_ID","PI_CACHE_RETENTION"])delete process.env[key];
Date.now=()=>1000000;
const model={id:"test",api:"anthropic-messages",provider:"anthropic",baseUrl:"https://example.test",reasoning:false,input:["text"],maxTokens:8192,contextWindow:200000,cost:{input:0,output:0,cacheRead:0,cacheWrite:0},headers:{"User-Agent":"fixture"}};
const inputs:any[]=[
 {name:"exchange and cache",calls:2},
 {name:"optional workspace service account",options:{env:{ANTHROPIC_WORKSPACE_ID:"workspace",ANTHROPIC_SERVICE_ACCOUNT_ID:"account"}}},
 {name:"explicit key bypasses federation",options:{apiKey:"key"}},
 {name:"header auth bypasses federation",options:{headers:{Authorization:"Bearer header"}}},
 {name:"model auth overrides minted token",model:{headers:{Authorization:"Bearer model"}}},
 {name:"explicit auth omission still exchanges",options:{headers:{Authorization:null}}},
 {name:"beta appended after override",options:{headers:{"anthropic-beta":"custom"}}},
 {name:"beta already present",options:{headers:{"anthropic-beta":"custom, oauth-2025-04-20"}}},
 {name:"beta deletion cannot suppress auth beta",options:{headers:{"anthropic-beta":null}}},
 {name:"non anthropic no federation",model:{provider:"custom"}},
 {name:"incomplete federation rejected",options:{env:{ANTHROPIC_FEDERATION_RULE_ID:""}}},
 {name:"insecure token endpoint rejected",model:{baseUrl:"http://example.test"}},
 {name:"empty identity rejected",identity:"  \n "},
 {name:"long identity rejected",identityRepeat:16385},
 {name:"token HTTP redaction",tokenStatus:403,tokenBody:'{"error":"denied","assertion":"fake-secret","access_token":"fake-access","error_description":"denied detail"}'},
 {name:"token 401 hint",tokenStatus:401,tokenBody:'{"error":"denied","refresh_token":"fake-refresh"}'},
 {name:"token 401 workspace hint",options:{env:{ANTHROPIC_WORKSPACE_ID:"workspace"}},tokenStatus:401,tokenBody:'{"error":"denied"}'},
 {name:"non JSON token response",tokenBody:"not json"},
 {name:"missing access token",tokenBody:'{"expires_in":3600,"assertion":"fake-secret"}'},
 {name:"missing expiry",tokenBody:'{"access_token":"fake-access"}'},
 {name:"unsupported type",tokenBody:'{"access_token":"fake-access","expires_in":3600,"token_type":"MAC"}'},
 {name:"mixed case bearer",tokenBody:'{"access_token":"fake-access","expires_in":"3600","token_type":"bEaReR"}'},
 {name:"zero expiry refreshes",calls:2,tokenBody:'{"access_token":"fake-access","expires_in":0}'},
 {name:"API 401 invalidates next call",calls:2,apiStatuses:[401,200],options:{maxRetries:2}},
 {name:"API retry reuses token",apiStatuses:[429,200],options:{maxRetries:1}},
 {name:"bounded token response",oversized:true},
 {name:"identity file rotates after invalidation",calls:2,apiStatuses:[401,200],rotateIdentity:true},
 {name:"expiry string null rejected",tokenBody:'{"access_token":"fake-access","expires_in":"null"}'},
 {name:"expiry boolean",tokenBody:'{"access_token":"fake-access","expires_in":true}'},
 {name:"expiry empty array",tokenBody:'{"access_token":"fake-access","expires_in":[]}'},
 {name:"expiry hex string",tokenBody:'{"access_token":"fake-access","expires_in":"0x1000"}'},
 {name:"numeric access token coerced",tokenBody:'{"access_token":123,"expires_in":3600}'},
 {name:"token trailing whitespace trimmed",tokenBody:'{"access_token":"fake-access  ","expires_in":3600}'},
];
const ignoredHeaders=["x-stainless-lang","x-stainless-package-version","x-stainless-os","x-stainless-arch","x-stainless-runtime","x-stainless-runtime-version"];
const cases=[];
const directory=mkdtempSync(join(tmpdir(),"pi-fed-"));const identityFile=join(directory,"identity");
try{for(const input of inputs){
 writeFileSync(identityFile,input.identity??(input.identityRepeat?"x".repeat(input.identityRepeat):"  fixture-assertion\n"));
 const chosen={...model,...input.model,headers:{...model.headers,...input.model?.headers}};
 const options={...input.options,env:{ANTHROPIC_FEDERATION_RULE_ID:"rule",ANTHROPIC_ORGANIZATION_ID:"organization",ANTHROPIC_IDENTITY_TOKEN_FILE:identityFile,...input.options?.env}};
 const requests:any[]=[];const results:any[]=[];let exchanges=0,apiCalls=0;
 const fetch=async(url:any,init:any)=>{
  const uri=new URL(url);const headers=Object.fromEntries(new Headers(init.headers));for(const key of ignoredHeaders)delete headers[key];requests.push({path:uri.pathname,query:uri.search.slice(1),headers,body:JSON.parse(init.body)});
  if(uri.pathname.endsWith("/oauth/token")){exchanges++;return new Response(input.oversized?"x".repeat((1<<20)+1):(input.tokenBody??JSON.stringify({access_token:`access-${exchanges}`,expires_in:3600})),{status:input.tokenStatus??200,headers:{"request-id":"token-request"}})}
  const status=input.apiStatuses?.[apiCalls++]??200;
  return new Response(status>=300?'{"error":{"message":"try again"}}':'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}\n\n',{status,headers:{"content-type":"text/event-stream","retry-after-ms":"0"}});
 };
 for(let i=0;i<(input.calls??1);i++){
  if(i===1&&input.rotateIdentity)writeFileSync(identityFile,"rotated-assertion");
  let result=null,error=null;try{const stream=streamSimple(chosen,normalizeContext({messages:[]}),{...options,fetch});result=await stream.result();for await(const _ of stream){}}catch(failure){error=(failure as Error).message}
  results.push({result,error});
 }
 cases.push({input,expected:JSON.parse(JSON.stringify({requests,results}).replaceAll(identityFile,"<identity-file>"))});
}}finally{rmSync(directory,{recursive:true,force:true})}
const file=new URL("pi-anthropic-federation.json",import.meta.url);const output=JSON.stringify({upstreamCommit:manifest.commit,sdkVersion:"0.129.0",model,ignoredHeaders,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic federation oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic federation cases`);
