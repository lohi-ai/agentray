import {execFileSync} from "node:child_process";
import {readFileSync,writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3",[new URL("../../third_party/pi/verify.py",import.meta.url).pathname],{stdio:"inherit"});
const manifest=JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json",import.meta.url),"utf8"));
const inputs=[
 '{"z":0,"a":1,"m":2}',
 '{"b":1,"a":2,"b":3,"c":4}',
 '{"10":10,"2":2,"01":1,"z":0,"4294967294":4,"4294967295":5,"0":0}',
 '{"z":0,"value":{"z":1,"a":2,"m":3},"a":1}',
 '{"value":[{"z":1,"a":2},[3,2,1]],"z":1,"a":2}',
 '{"__proto__":{"z":1},"constructor":2,"toString":3,"z":4,"a":5}',
 'null',
 String.raw`{"\ud801":1,"\ud800":2,"z":3,"value":{"\ud801":1,"\ud800":2}}`,
];
const cases=[];
for(const raw of inputs)for(const phase of ["plain","merge","undefined","input","snapshot","nested","reinsert","reader"]){
 let attrs=JSON.parse(raw);const recorder=new InMemoryTelemetryContext();let before:any,after:any,retained:any;
 await recorder.startSpan({name:"order",attributes:attrs},span=>{
  span.addEvent("initial",attrs);
  retained=recorder.getSpans();before=JSON.stringify(retained);
  const patch=JSON.parse('{"y":1,"b":2,"a":3}');
  if(phase==="merge")span.setAttributes(patch);
  if(phase==="undefined")span.setAttributes({y:undefined,b:2,z:undefined,a:3});
  if(phase==="input"||phase==="reinsert"){
   attrs??={};const key=Object.keys(attrs)[0];if(key!==undefined){const value=attrs[key];delete attrs[key];attrs[key]=value;}
   attrs.late=9;if(phase==="reinsert")span.setAttributes(attrs);
  }
  if(phase==="snapshot"){
   const values=retained[0].attributes,key=Object.keys(values)[0];if(key!==undefined){const value=values[key];delete values[key];values[key]=value;}values.late=9;
  }
  if(phase==="nested"){
   let value=attrs?.value;if(Array.isArray(value))value=value[0];
   if(value&&typeof value==="object"){const key=Object.keys(value)[0];const previous=value[key];delete value[key];value[key]=previous;value.late=9;}
  }
  if(phase==="reader")span.setAttributes({get outer(){span.setAttributes({inner:1});return patch;}});
  after=JSON.stringify(recorder.getSpans());
 });
 cases.push({raw,phase,expected:{before,after,retained:JSON.stringify(retained),settled:JSON.stringify(recorder.getSpans())}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n",destination=new URL("pi-attribute-order.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Attribute order fixtures changed");}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} attribute order cases against Pi ${manifest.commit}`);
