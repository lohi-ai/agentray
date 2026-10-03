// Exercise decoded strings/keys through the unchanged Pi recorder.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
// Encode strings and property names as JSON text inside the projection, so
// fixture readers cannot silently replace surrogates or merge distinct keys.
function inspect(value: any): any {
  if (typeof value === "string") return {string: JSON.stringify(value)};
  if (Array.isArray(value)) return value.map(inspect);
  if (value === null || typeof value !== "object") return value;
  return {object: Object.fromEntries(Object.entries(value).map(([key, child]) => [JSON.stringify(key), inspect(child)]))};
}
const inputs = [
 String.raw`{"value":"\ud800"}`, String.raw`{"value":"\ud801"}`,
 String.raw`{"value":"\udbff"}`, String.raw`{"value":"\udc00"}`,
 String.raw`{"value":"\udfff"}`, String.raw`{"value":"\ufffd"}`,
 String.raw`{"value":"\ud800\udc00"}`, String.raw`{"value":"\udbff\udfff"}`,
 String.raw`{"value":"\udc00\ud800"}`, String.raw`{"value":"\ud800\ud801\udc00"}`,
 String.raw`{"value":"before\ud800after\udfff"}`,
 String.raw`{"value":"\u0000\b\f\n\r\t\"\\/<>\u2028\u2029"}`,
 String.raw`{"value":"xin chào [emoji]"}`,
 String.raw`{"\ud800":1,"\ud801":2,"\udfff":3,"\ufffd":4}`,
 String.raw`{"\ud800":1,"\uD800":2,"\ud801":3}`,
 String.raw`{"[emoji]":1,"\ud83d\ude00":2,"\ud83d":3,"\ude00":4}`,
 String.raw`{"value":{"\ud800":"\ud801","\ud801":"\ud800","\ufffd":"\ufffd"}}`,
 String.raw`{"value":["\ud800",["\udfff"],{"\ud800":"\udfff"},"[emoji]"]}`,
 String.raw`{"__proto__":{"\ud800":"\ud801"},"value":{"__proto__":{"\ud800":"\ud801"}}}`,
 String.raw`{"0":"\ud800","2":"\ud801","10":"\udfff","\ue000":"\ud800","[emoji]":"\ud801"}`,
];
const cases=[];
for (const input of inputs) for (const phase of ["start","merge","event"]) {
 const raw=input.replaceAll("[emoji]",String.fromCodePoint(0x1f600));
 const attributes=JSON.parse(raw), recorder=new InMemoryTelemetryContext();
 let active:any;
 await recorder.startSpan({name:"unicode",attributes:phase==="start"?attributes:{kept:true}},span=>{
  if(phase==="merge")span.setAttributes(attributes);
  if(phase==="event")span.addEvent("unicode",attributes);
  const spans=recorder.getSpans();
  active={memory:inspect(spans),wire:inspect(JSON.parse(JSON.stringify(spans)))};
 });
 const spans=recorder.getSpans();
 cases.push({raw,phase,expected:{input:inspect(attributes),active,settled:{memory:inspect(spans),wire:inspect(JSON.parse(JSON.stringify(spans)))}}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination=new URL("pi-attribute-unicode.json",import.meta.url);
if(process.argv.includes("--check")){
 if(readFileSync(destination,"utf8")!==output)throw new Error("Attribute Unicode fixtures changed");
}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} attribute Unicode cases against Pi ${manifest.commit}`);
