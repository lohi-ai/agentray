import {readFileSync,writeFileSync} from "node:fs";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
const {toToolDeclaration,declarationsEqual,getToolStateChanges} = await import(new URL("utils/transcript.ts",aiRoot).pathname);
const pairs = [
 ["distinct lone surrogates",String.raw`{"value":"\ud800"}`,String.raw`{"value":"\udc00"}`],
 ["surrogate versus replacement",String.raw`{"value":"\ud800"}`,String.raw`{"value":"\ufffd"}`],
 ["distinct surrogate keys",String.raw`{"\ud800":1}`,String.raw`{"\udc00":1}`],
 ["surrogate key collision",String.raw`{"\ud800":1,"\udc00":2}`,String.raw`{"\ud800":2}`],
 ["surrogate escape casing",String.raw`{"value":"\uD800"}`,String.raw`{"value":"\ud800"}`],
 ["index key order",'{"z":1,"10":2,"2":3}','{"2":3,"10":2,"z":1}'],
 ["ordinary key order",'{"z":1,"a":2}','{"a":2,"z":1}'],
 ["duplicate keys",'{"a":1,"b":2,"a":3}','{"a":3,"b":2}'],
 ["number rounding",'{"value":9007199254740993}','{"value":9007199254740992}'],
 ["number overflow",'{"value":1e309}','{"value":null}'],
 ["zero spelling",'{"value":-0}','{"value":0}'],
 ["HTML and separators",String.raw`{"value":"\u003c\u003e\u0026\u2028\u2029"}`,JSON.stringify({value:"<>&\u2028\u2029"})],
];
const cases = [];
for (const [name,left,right] of pairs) for (const field of ["parameters","constrainedSampling"]) {
 const tool = (raw: string) => ({name:"tool",description:"test",parameters:{type:"object"},[field]:JSON.parse(raw)});
 const a = tool(left), b = tool(right);
 cases.push({name:`${field}/${name}`,field,left,right,expected:{left:JSON.stringify(toToolDeclaration(a)[field]),right:JSON.stringify(toToolDeclaration(b)[field]),equal:declarationsEqual(a,b),changed:getToolStateChanges([a],[b]).toolsAdded.length > 0}});
}
const output = JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination = new URL("pi-declarations.json",import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination,"utf8") !== output) throw new Error("Declaration fixtures differ; regenerate and review");
} else writeFileSync(destination,output);
console.log(`Verified ${cases.length} declaration cases against Pi ${manifest.commit}`);
