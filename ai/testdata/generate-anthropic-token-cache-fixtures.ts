import {readFileSync,writeFileSync} from "node:fs";
import {manifest} from "../../agentcore/engine/testdata/oracle.ts";
import {TokenCache} from "../../third_party/pi/node_modules/@anthropic-ai/sdk/src/lib/credentials/token-cache.ts";
const inputs:any[]=[
 {name:"fresh cached",actions:["get","get"]},
 {name:"no expiry cached",responses:[{token:"forever",expiresAt:null}],actions:["get",100000,"get"]},
 {name:"above advisory threshold",actions:["get",1879,"get"]},
 {name:"advisory boundary",actions:["get",1880,"get","get"]},
 {name:"above mandatory threshold",actions:["get",1969,"get","get"]},
 {name:"mandatory boundary",actions:["get",1970,"get"]},
 {name:"expired",actions:["get",2001,"get"]},
 {name:"invalidation forces",actions:["get","invalidate","get","get"]},
 {name:"invalidation before first get",actions:["invalidate","get"]},
 {name:"first failure retried",responses:[{error:"exchange failed"}],actions:["get","get"]},
 {name:"mandatory failure propagates keeps cache",responses:[{token:"old",expiresAt:2000},{error:"mandatory failed"}],actions:["get",1970,"get",1000,"get"]},
 {name:"advisory failure keeps cache and backs off",responses:[{token:"old",expiresAt:2000},{error:"advisory failed"}],actions:["get",1880,"get","get",1884,"get",1885,"get","get"]},
 {name:"mandatory bypasses advisory backoff",responses:[{token:"old",expiresAt:2000},{error:"advisory failed"}],actions:["get",1969,"get",1970,"get"]},
 {name:"forced bypasses advisory backoff",responses:[{token:"old",expiresAt:2000},{error:"advisory failed"}],actions:["get",1880,"get","invalidate","get"]},
 {name:"failed forced refresh retries unforced",responses:[{token:"old",expiresAt:2000},{error:"forced failed"}],actions:["get","invalidate","get","get"]},
 {name:"initial advisory backoff near epoch",responses:[{token:"old",expiresAt:100}],actions:[0,"get","get",5,"get","get"]},
];
const cases=[];
for(const input of inputs){
 let now=1000;Date.now=()=>now*1000;const calls:boolean[]=[];const errors:string[]=[];
 const cache=new TokenCache(async(options)=>{calls.push(options?.forceRefresh??false);const response=input.responses?.[calls.length-1];if(response?.error)throw new Error(response.error);return response??{token:`token-${calls.length}`,expiresAt:now+1000}},(error:any)=>errors.push(error.message));
 const events=[];
 for(const action of input.actions){
  if(typeof action==="number"){now=action;continue}
  if(action==="invalidate"){cache.invalidate();continue}
  let value=null,error=null;try{value=await cache.getToken()}catch(failure){error=(failure as Error).message}
  await new Promise(resolve=>setTimeout(resolve,0));
  events.push({value,error,calls:[...calls]});
 }
 cases.push({input,expected:events});
}
const sdkVersion=JSON.parse(readFileSync(new URL("../../third_party/pi/node_modules/@anthropic-ai/sdk/package.json",import.meta.url),"utf8")).version;
const file=new URL("pi-anthropic-token-cache.json",import.meta.url);const output=JSON.stringify({upstreamCommit:manifest.commit,sdkVersion,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Anthropic token cache oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Anthropic token cache cases against SDK ${sdkVersion}`);
