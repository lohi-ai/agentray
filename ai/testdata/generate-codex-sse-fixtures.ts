import {readFileSync,writeFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
plugin({name:"codex-sse-oracle",setup(build){build.onLoad({filter:/\/api\/openai-codex-responses\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport {parseSSE as oracleSSE};\n"}))}});
const {oracleSSE}=await import(new URL("api/openai-codex-responses.ts",aiRoot).pathname);
const inputs:any[]=[
 {name:"empty",wire:""},
 {name:"no body",wire:null},
 {name:"one frame",wire:'data: {"type":"a"}\n\n'},
 {name:"residual EOF",wire:'data:{"type":"last"}'},
 {name:"comments ignored",wire:': comment\nevent: ignored\ndata: {"x":1}\n\n'},
 {name:"multiline",wire:'data: {"a":\ndata: 1}\n\n'},
 {name:"done is skipped",wire:'data: [DONE]\n\ndata:{"after":true}\n\n'},
 {name:"empty data skipped",wire:'data:\n\ndata:  \n\n'},
 {name:"CRLF one residual frame",wire:'data: {"x":1}\r\n\r\n'},
 {name:"frame prefix exact",wire:' data: {"ignored":true}\nData: {"ignored":true}\ndata: {"ok":true}\n\n'},
 {name:"BOM initial",wire:'\ufeffdata: {"ok":true}\n\n'},
 {name:"BOM in data trimmed",wire:'data: \ufeff{"ok":true}\ufeff\n\n'},
 {name:"BOM in later line not stripped",wire:'data: {"first":true}\n\n\ufeffdata:{"ignored":true}\n\n'},
 ...[1,2,3,7].map(chunkSize=>({name:`split unicode ${chunkSize}`,wire:'data: {"text":"😀é漢字"}\n\ndata: {"next":2}',chunkSize})),
];
const cases=[];
for(const input of inputs){
 const bytes=new TextEncoder().encode(input.wire??"");let offset=0;
 const response=new Response(input.wire===null?null:new ReadableStream({pull(controller){if(offset>=bytes.length){controller.close();return}const end=Math.min(bytes.length,offset+(input.chunkSize??bytes.length));controller.enqueue(bytes.slice(offset,end));offset=end}}));
 const expected=[];for await(const event of oracleSSE(response))expected.push(event);
 cases.push({input,expected});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";const file=new URL("pi-codex-sse.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(file,"utf8")!==output)throw new Error("Codex SSE oracle changed")}else writeFileSync(file,output);
console.log(`Verified ${cases.length} Codex SSE cases against Pi ${manifest.commit}`);
