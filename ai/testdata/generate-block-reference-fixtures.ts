import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {transformMessages}=await import(new URL("api/transform-messages.ts",aiRoot).pathname);
Date.now=()=>100;
const cases=[];
for(const role of ["assistant","user","toolResult"]) for(const vision of [false,true]) for(const mode of ["same","cross","normalize"]) {
 const content=role==="assistant" ? [
  {type:"text",text:"answer",textSignature:"text-signature"},
  {type:"thinking",thinking:"reason"},
  {type:"thinking",thinking:"",thinkingSignature:"signed"},
  {type:"thinking",thinking:"redacted",redacted:true},
  {type:"toolCall",id:"signed-call",name:"echo",arguments:{},thoughtSignature:"signed"},
  {type:"toolCall",id:"plain-call",name:"echo",arguments:{}},
  {type:"custom",text:"extension"},
 ] : [{type:"image",data:"a",mimeType:"image/png"},{type:"text",text:"caption"},{type:"image",data:"b",mimeType:"image/png"},{type:"image",data:"c",mimeType:"image/png"}];
 const messages:any[]=[{role,content,timestamp:1,...(role==="assistant"?{api:"test",provider:"test",model:"source",stopReason:"stop",usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}}}:role==="toolResult"?{toolCallId:"call",toolName:"echo",isError:false}:{})}];
 const model={id:mode==="same"?"source":"target",api:"test",provider:"test",input:vision?["text","image"]:["text"]};
 const output=transformMessages(messages,model,mode==="normalize"?(id:string)=>"normalized-"+id:undefined);
 cases.push({input:{name:`${role}/${vision?"vision":"text"}/${mode}`,messages,model,normalize:mode==="normalize"},expected:{messages:output,indices:output.map((m:any)=>m.content.map((block:any)=>content.indexOf(block)))}});
}
const serialized=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination=new URL("pi-block-references.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==serialized)throw new Error("Block reference fixtures changed");}
else writeFileSync(destination,serialized);
console.log(`Verified ${cases.length} block-reference cases against Pi ${manifest.commit}`);
