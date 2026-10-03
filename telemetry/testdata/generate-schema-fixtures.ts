// Development oracle: schema data and bound starters from the unchanged Pi source.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext, NOOP_TELEMETRY_CONTEXT, defineTelemetrySchema, createTypedSpanStarter } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";
import { createTelemetryAdapterConformance } from "../../third_party/pi/upstream/packages/telemetry/src/testing/index.ts";
execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], {stdio:"inherit"});
const manifest=JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json",import.meta.url),"utf8"));
const definitions:any[]=[{
 version:1,spans:{operation:{description:"Operation",parents:{kind:"root_or_external"},startAttributes:{
  kind:{type:"string",description:"Kind",required:true,values:["read","write"],examples:["read"],sensitive:false,cardinality:"low"},
  count:{type:"number",description:"Count",required:false,values:[0,1,2.5],examples:[2.5]},
  enabled:{type:"boolean",description:"Enabled",required:false,values:[false,true],examples:[false]},
  labels:{type:"string[]",description:"Labels",required:false,elementValues:["a","b"],examples:[[],["a","b"]],sensitive:true,cardinality:"high"},
  counts:{type:"number[]",description:"Counts",required:false,elementValues:[0,1.5],examples:[[0,1.5]]},
  flags:{type:"boolean[]",description:"Flags",required:false,elementValues:[true,false],examples:[[true,false]]}
 },endAttributes:{done:{type:"boolean",description:"Done"}},events:{result:{description:"Result",attributes:{outcome:{type:"string",description:"Outcome",required:true,values:["ok","error"]}}}},status:{default:"ok",errorWhen:"Operation fails"}}}
},{version:3,spans:{request:{description:"Request",parents:{kind:"spans",spans:["operation"]},startAttributes:{provider:{type:"string",description:"Provider",required:true}},endAttributes:{response:{type:"string",description:"Response"}},events:{},status:{default:"ok",errorWhen:"Request fails"}},empty:{description:"Empty",parents:{kind:"spans",spans:[]},startAttributes:{},endAttributes:{},status:{default:"ok",errorWhen:"Fails"}},any:{description:"Any",parents:{kind:"any"},startAttributes:{},endAttributes:{},status:{default:"ok",errorWhen:"Fails"}}}}];
const schemas=definitions.map(schema=>defineTelemetrySchema(schema));
if(schemas.some((schema,index)=>schema!==definitions[index]))throw new Error("schema identity changed");
const inputs:any[]=[
 {name:"cross-schema parentage",actions:[{op:"span",name:"operation",attributes:{kind:"read"},actions:[{op:"span",name:"request",attributes:{provider:"example"},actions:[{op:"attributes",attributes:{response:"cached"}},{op:"event",name:"result",attributes:{outcome:"ok"}}],result:42}],result:42}]},
 {name:"runtime does not validate vocabulary",actions:[{op:"span",name:"undeclared",attributes:{unknown:1},actions:[{op:"event",name:"undeclared-event",attributes:{extra:true}},{op:"attributes",attributes:{extra:"retained"}}]}]},
 {name:"saved child starter after settlement",actions:[{op:"span",name:"operation",actions:[]},{op:"span",name:"late",target:"operation",actions:[{op:"span",name:"late-grandchild"}],result:7}]},
 {name:"no ambient parent",actions:[{op:"span",name:"operation",actions:[{op:"span",name:"request",actions:[{op:"span",name:"root",target:"root"}]}]},{op:"span",name:"second-root"}]},
 {name:"no-op typed starter",noop:true,actions:[{op:"span",name:"operation",actions:[{op:"span",name:"request",result:9}],result:42}]},
 {name:"duplicate schemas have no runtime validation",duplicate:true,actions:[{op:"span",name:"operation",attributes:{kind:"write"}}]},
 {name:"schema input is not read",unreadable:true,actions:[{op:"span",name:"operation",actions:[{op:"span",name:"request"}]}]},
];
const cases=[];
for(const input of inputs){
 const recorder=new InMemoryTelemetryContext();
 const values=input.unreadable?new Proxy([], {get(){throw new Error("schema read")},ownKeys(){throw new Error("schema enumeration")}}):input.duplicate?[schemas[0],schemas[0]]:schemas;
 const root=createTypedSpanStarter(input.noop?NOOP_TELEMETRY_CONTEXT:recorder,values as any) as any;
 const retained=new Map<string,any>([["root",root]]);
 const results:any[]=[];
 async function run(actions:any[],starter:any,span?:any){
  for(const action of actions){
   if(action.op==="span"){
    const result=await (action.target?retained.get(action.target):starter)(action.name,action.attributes??{},async(child:any,startChild:any)=>{
     retained.set(action.name,startChild);
     await run(action.actions??[],startChild,child);
     return action.result??null;
    });
    results.push({name:action.name,result});
   }else if(action.op==="attributes")span.setAttributes(action.attributes);
   else if(action.op==="event")span.addEvent(action.name,action.attributes);
  }
 }
 await run(input.actions,root);
 cases.push({input,expected:{spans:recorder.getSpans(),results}});
}
const conformance=createTelemetryAdapterConformance(async()=>{
 const context=new InMemoryTelemetryContext();
 return {context,getSpans:async()=>context.getSpans(),[Symbol.asyncDispose]:async()=>{}};
});
for(const test of conformance)await test.run();
const output=JSON.stringify({upstreamCommit:manifest.commit,schemas,cases,conformance:conformance.map(({group,name})=>({group,name}))},null,2)+"\n";
const destination=new URL("pi-schema.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Pi schema fixtures changed; regenerate and review")}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} schema/starter cases and ${conformance.length} upstream conformance cases against Pi ${manifest.commit}`);
