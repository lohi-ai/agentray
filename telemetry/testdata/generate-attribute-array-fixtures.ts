import {execFileSync} from "node:child_process";
import {readFileSync,writeFileSync} from "node:fs";
import {InMemoryTelemetryContext} from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3",[new URL("../../third_party/pi/verify.py",import.meta.url).pathname],{stdio:"inherit"});
const manifest=JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json",import.meta.url),"utf8"));
const cases=[];
for(const placement of ["outer","object","array","shared"])for(const sparse of [false,true])for(const target of ["input","snapshot"])for(const operation of ["append","set","delete","shrink","grow","pop","undefined"]){
 const input={placement,sparse,target,operation};const original:any[]=[1,2];if(sparse){delete original[0];original.length=4;}
 const value=placement==="outer"?original:placement==="object"?{inner:original}:placement==="array"?[original]:{left:original,right:original};
 const select=(value:any):any[]=>placement==="outer"?value:placement==="object"?value.inner:placement==="array"?value[0]:value.left;
 const describe=(array:any[])=>({length:array.length,keys:Object.keys(array),values:Array.from(array,value=>value===undefined?"undefined":value===null?"null":String(value))});
 const project=(spans:any[])=>({wire:JSON.stringify(spans),arrays:spans.map(span=>{
  const attribute=select(span.attributes.value),event=select(span.events[0].attributes.value);
  return {attribute:describe(attribute),event:describe(event),attributeInput:attribute===original,eventInput:event===original,same:attribute===event,...(placement==="shared"?{shared:span.attributes.value.left===span.attributes.value.right}:{})};
 })});
 const recorder=new InMemoryTelemetryContext();let before:any,after:any,retained:any,result:any=null;
 await recorder.startSpan({name:"array",attributes:{value}},span=>{
  span.addEvent("copy",{value});retained=recorder.getSpans();before=project(retained);
  const array=target==="input"?original:select(retained[0].attributes.value);
  if(operation==="append")result=array.push(3,4);
  if(operation==="set")array[6]=7;
  if(operation==="delete")delete array[1];
  if(operation==="shrink")array.length=1;
  if(operation==="grow")array.length=6;
  if(operation==="pop"){const popped=array.pop();result=popped===undefined?"undefined":popped;}
  if(operation==="undefined"){array[0]=undefined;array[1]=null;}
  after=project(recorder.getSpans());
 });
 cases.push({input,expected:{before,after,retained:project(retained),settled:project(recorder.getSpans()),original:describe(original),result}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n",destination=new URL("pi-attribute-arrays.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Attribute array fixtures changed");}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} attribute array cases against Pi ${manifest.commit}`);
