import {readFileSync,writeFileSync} from "node:fs";
import {root,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const api=await import(new URL("upstream/packages/ai/src/api/constrained-sampling.ts",root).pathname);
const schema={type:"object",properties:{z:{type:"string"},a:{type:"number"},"10":{type:"integer"},"2":{type:"boolean"}},required:["z"]};
const inputs:any[]=[
 {name:"property order and optional null",schema},{name:"nested schemas",schema:{type:"object",properties:{nested:{type:"object",properties:{x:{type:"array",items:{type:"string"}}}},nullable:{anyOf:[{type:"string"},{type:"null"}]},constant:{const:null},enum:{enum:[1,null]}}}},
 {name:"empty object",schema:{type:"object"}},{name:"root boolean",schema:true},{name:"root scalar",schema:{type:"string"}},
 ...["$ref","$defs","definitions","allOf","oneOf","patternProperties","dependentSchemas","dependencies","unevaluatedProperties","propertyNames","contains","prefixItems","not","if","then","else"].map(key=>({name:`reject ${key}`,schema:{type:"object",[key]:null}})),
 ...[
  {properties:true},{required:null},{required:[1]},{required:["missing"]},{additionalProperties:true},{additionalProperties:{}},
  {anyOf:[]},{anyOf:{}},{anyOf:[{type:"object"}]},{anyOf:[{type:["null","array"]}]},{anyOf:[{properties:{}}]},
  {items:[]},{items:false},{properties:{bad:false}},{properties:{bad:{properties:{}}}},
 ].map((extra,i)=>({name:`invalid schema ${i}`,schema:{type:"object",...extra}})),
 {name:"extra strict restriction",schema:{type:"object",properties:{x:{type:"string",format:"email"}}},rejectKey:"format"},
 ...[false,true].flatMap(supported=>["prefer","require"].flatMap(strict=>[false,true].map(invalid=>({name:`strict ${supported} ${strict} ${invalid}`,mode:"strict",supported,tool:{name:"tool",description:"Tool",parameters:invalid?{type:"object",oneOf:[]}:schema,constrainedSampling:{type:"json_schema",strict}}})))),
 {name:"strict not requested",mode:"strict",supported:true,tool:{name:"tool",parameters:schema}},
 ...[
  {variants:{openai_lark:"start: /.+/",openai_regex:".*"}},
  {variants:{openai_lark:" ",openai_regex:".*"}},
  {variants:{}},
 ].flatMap((grammar,i)=>[false,true].map(supported=>({name:`grammar variants ${i} ${supported}`,mode:"grammar",supported,tool:{name:"grammar",parameters:{type:"object",properties:{input:{type:"string"}},required:["input"]},constrainedSampling:{type:"grammar",...grammar}}}))),
 ...[{type:"array"},{type:"object"},{type:"object",required:["x","y"]},{type:"object",required:[null]},{type:"object",required:["x"]},{type:"object",required:["x"],properties:{x:{type:"number"}}}].map((parameters,i)=>({name:`grammar schema ${i}`,mode:"grammar",supported:true,tool:{name:"grammar",parameters,constrainedSampling:{type:"grammar",variants:{openai_regex:".*"}}}})),
 ...[{}, {input:null},{input:3},{input:""},{input:"<>&\u2028\u2029\\u2028\n"}].map((args,i)=>({name:`grammar input ${i}`,mode:"input",args,property:"input"})),
 {name:"grammar deltas escape exact strings",mode:"delta",property:"a\"\n<>&\u2028",steps:[{input:"",close:false},{input:"a\n\"",close:false},{input:"a\n\"<>&\u2028\u2029\\u2028",close:false},{input:"a\n\"<>&\u2028\u2029\\u2028",close:true},{input:"a\n\"<>&\u2028\u2029\\u2028",close:true},{input:"changed",close:true}]},
 {name:"grammar nonmonotonic delta",mode:"delta",property:"input",steps:[{input:"abc",close:false},{input:"ab",close:false},{input:"abcd",close:true},{input:"abcd",close:false}]},
 {name:"empty grammar close",mode:"delta",property:"input",steps:[{input:"",close:true}]},
];
const cases=[];
for(const input of inputs){
 let result:any=null,error:string|null=null;
 const original=JSON.stringify(input);
 const check=input.rejectKey?(key:string)=>key===input.rejectKey:undefined;
 try{
  switch(input.mode){
   case "strict":{const strict=api.resolveJsonSchemaStrictSampling(input.tool,input.supported,check);result={strict:strict??null,parameters:api.getJsonSchemaToolParameters(input.tool,strict)};break}
   case "grammar":result={grammar:api.resolveGrammarConstrainedSampling(input.tool,input.supported)??null,properties:Object.fromEntries(api.createGrammarToolInputProperties([input.tool],input.supported))};break;
   case "input":result=api.getGrammarToolInput("grammar",input.args,input.property);break;
   case "delta":{const buffer={input:"",started:false,closed:false};result=[];for(const step of input.steps){let delta=null,error=null;try{delta=api.appendGrammarToolInputJsonDelta(buffer,input.property,step.input,step.close)??null}catch(failure){error=(failure as Error).message}result.push({delta,error,buffer:{...buffer}})}break}
   default:{const schema=api.makeStrictJsonSchema(input.schema,check);result={schema,second:api.makeStrictJsonSchema(schema,check)}}
  }
 }catch(failure){error=(failure as Error).message}
 if(JSON.stringify(input)!==original)throw new Error("oracle mutated input");
 cases.push({input,expected:{result,error}});
}
const file=new URL("pi-constrained-sampling.json",import.meta.url);
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Constrained sampling oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} constrained sampling cases against Pi ${manifest.commit}`);
