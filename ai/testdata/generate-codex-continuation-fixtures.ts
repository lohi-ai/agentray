import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"codex-continuation-oracle",setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {buildCachedWebSocketRequestBody as oracleBody};\n"}))}});
const {oracleBody}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const user={role:'user',content:'question'};const answer={type:'message',id:'answer',role:'assistant',content:[{type:'output_text',text:'answer'}]};
const lastRequestBody={model:'test',store:false,input:[user]};const continuation={lastRequestBody,lastResponseId:'response',lastResponseItems:[answer]};
const body={model:'test',store:false,input:[user,answer,{role:'user',content:'next'}]};
const inputs:any[]=[
 {name:'matching prefix',body,continuation},
 {name:'no continuation',body},
 {name:'empty delta',body:{...body,input:[user,answer]},continuation},
 {name:'short prefix',body:{...body,input:[user]},continuation},
 {name:'changed input',body:{...body,input:[{...user,content:'changed'},answer]},continuation},
 {name:'changed output',body:{...body,input:[user,{...answer,id:'other'}]},continuation},
 {name:'changed model',body:{...body,model:'other'},continuation},
 {name:'changed store',body:{...body,store:true},continuation},
 {name:'added option',body:{...body,temperature:0},continuation},
 {name:'missing option',body:{model:'test',input:body.input},continuation},
 {name:'settings key order changed',body:{store:false,model:'test',input:body.input},continuation},
 {name:'input key order changed',body:{...body,input:[{content:'question',role:'user'},answer]},continuation},
 {name:'old previous id ignored',body:{...body,previous_response_id:'old'},continuation},
 {name:'prior previous id ignored',body,continuation:{...continuation,lastRequestBody:{...lastRequestBody,previous_response_id:'old'}}},
 {name:'empty response id invalidates',body,continuation:{...continuation,lastResponseId:''}},
 ...[undefined,null,[]].flatMap(input=>[undefined,null,[]].map(old=>({name:`empty input ${JSON.stringify(input)} prior ${JSON.stringify(old)}`,body:{model:'test',input},continuation:{lastRequestBody:{model:'test',input:old},lastResponseId:'response',lastResponseItems:[]}}))),
];
const cases=[];for(const input of inputs){const entry={continuation:structuredClone(input.continuation)};const before=JSON.stringify(input);const body=oracleBody(entry,input.body);if(JSON.stringify(input)!==before)throw new Error('input mutated');cases.push({input,expected:{body,retained:!!entry.continuation}})}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+'\n';const file=new URL('pi-codex-continuation.json',import.meta.url);
if(process.argv.includes('--check')){if(readFileSync(file,'utf8')!==output)throw new Error('Codex continuation oracle changed')}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex continuation cases against Pi ${manifest.commit}`);
