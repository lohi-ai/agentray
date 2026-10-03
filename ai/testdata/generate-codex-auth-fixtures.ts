import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"codex-auth-oracle",setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {extractAccountId as oracleAccount,resolveCodexUrl as oracleURL,buildSSEHeaders as oracleSSE,buildWebSocketHeaders as oracleWS};\n"}))}});
const {oracleAccount,oracleURL,oracleSSE,oracleWS}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const token=(claim:any)=>"header."+Buffer.from(JSON.stringify({"https://api.openai.com/auth":{chatgpt_account_id:claim}})).toString("base64")+".signature";
const tokens=[...['account','',null,0,1,false,true,['a','b'],{},'é'].map(token),"bad","a.b.c.d","a.@@.b","a.e30=.b","a.bnVsbA==.b",token("account").replace(/=/g,""),token("account").replace(".",". \n")];
const accounts=tokens.map(token=>{let account=null,error=null;try{account=String(oracleAccount(token))}catch(e){error=(e as Error).message}return {token,account,error}});
const urls=[undefined,""," \t\ufeff","https://chatgpt.com/backend-api","https://example.test/","https://example.test/codex///","https://example.test/codex/responses///"," https://example.test/x ","https://example.test/x?query=1"].map(base=>({base,url:oracleURL(base)}));
const inputs:any[]=[
 {name:"defaults"},
 {name:"session",session:"session"},
 {name:"empty session",session:""},
 {name:"model values",initial:{"X-Custom":"  value  ","Accept":"model","Content-Type":"model","OpenAI-Beta":"model","Session-Id":"model"}},
 {name:"override/delete",initial:{"X-Custom":"model","X-Delete":"delete","session-id":"initial"},additional:{"x-custom":"request","x-delete":null,"session-id":null}},
 {name:"case collisions",initial:{"X-Custom":"one","x-custom":"two"},additional:{"X-Other":"a","x-other":"b"}},
 {name:"auth protected",initial:{Authorization:"model","chatgpt-account-id":"model",originator:"model","User-Agent":"model"},additional:{authorization:null,"chatgpt-account-id":"request",originator:"request","user-agent":null}},
 {name:"model null is literal",initial:{"X-Custom":null}},
 {name:"session supersedes both",session:"bound",initial:{"session-id":"model","x-client-request-id":"model"},additional:{"session-id":"request","x-client-request-id":"request"}},
];
const headers=[];
for(const input of inputs)for(const websocket of [false,true]){
 const h=websocket?oracleWS(input.initial,input.additional,"account","token",input.session??"request"):oracleSSE(input.initial,input.additional,"account","token",input.session);
 if(!h.get("user-agent")?.startsWith("pi ("))throw new Error("missing Pi platform agent");h.set("user-agent","<platform>");
 headers.push({input:{...input,websocket,...(websocket?{session:input.session??"request"}:{})},expected:Object.fromEntries(h.entries())});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,accounts,urls,headers},null,2)+"\n";const file=new URL("pi-codex-auth.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Codex auth oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${accounts.length+urls.length+headers.length} Codex auth/URL/header cases against Pi ${manifest.commit}`);
