import {execFileSync} from "node:child_process";
import {readFileSync,writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3",[new URL("../../third_party/pi/verify.py",import.meta.url).pathname],{stdio:"inherit"});
const manifest=JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json",import.meta.url),"utf8"));
const cases=[];
for(const kind of ["object","array","shared-object","shared-array"])for(const mode of ["explicit","automatic"])for(const phase of ["plain","input-nested","snapshot-nested","input-replace","snapshot-replace","settled-nested"]){
 const input={kind,mode,phase};
 const raw=kind.endsWith("object")?'{"name":{"z":1,"a":2},"message":{"z":1,"a":2}}':'{"name":[1,2],"message":[1,2]}';
 const values=JSON.parse(raw);if(kind.startsWith("shared"))values.message=values.name;
 const error:any=mode==="automatic"?Object.assign(new Error("original"),values):values;
 const recorder=new InMemoryTelemetryContext();let retained:any,before:any,active:any,caught:any;
 const mutate=(value:any)=>{if(Array.isArray(value))value.push(3);else{value.late=3;delete value.z;value.z=4;}};
 const inspect=(status:any)=>({wire:JSON.stringify(status),nameInput:status.error.name===error.name,messageInput:status.error.message===error.message,same:status.error.name===status.error.message});
 const action=()=>{
  if(phase==="input-nested")mutate(error.name);
  if(phase==="snapshot-nested")mutate(retained.error.name);
  if(phase==="input-replace"){error.name="";error.message=null;}
  if(phase==="snapshot-replace"){retained.error.name="";retained.error.message=undefined;}
 };
 try{await recorder.startSpan({name:"reference"},span=>{
  if(mode==="automatic")throw error;
  span.setStatus({status:"error",error});retained=recorder.getSpans()[0].status;before=inspect(retained);action();active=inspect(recorder.getSpans()[0].status);
 });}catch(value){caught=value;}
 if(mode==="automatic"){retained=recorder.getSpans()[0].status;before=inspect(retained);action();active=inspect(recorder.getSpans()[0].status);}
 if(phase==="settled-nested")mutate(error.name);
 cases.push({input,raw,expected:{before,active,retained:inspect(retained),settled:inspect(recorder.getSpans()[0].status),input:JSON.stringify({name:error.name,message:error.message}),failurePreserved:mode==="automatic"?caught===error:caught===undefined}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n",destination=new URL("pi-status-references.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Status reference fixtures changed");}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} status reference cases against Pi ${manifest.commit}`);
