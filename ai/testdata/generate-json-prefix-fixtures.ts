// Exact serialized results, including property identity that decoding into Go
// strings would erase. Exercise every Unicode scalar boundary of each input.
import {createHash} from "node:crypto";
import {readFileSync, writeFileSync} from "node:fs";
import {aiRoot, manifest, root} from "../../agentcore/engine/testdata/oracle.ts";
if (Bun.version !== "1.3.14") throw new Error("JSON prefix oracle requires pinned Bun 1.3.14");
const dependency = new URL("node_modules/partial-json/dist/index.js", root);
const partialJSONSHA256 = createHash("sha256").update(readFileSync(dependency)).digest("hex");
if (partialJSONSHA256 !== "23f750aa1170c830b7ef180135852317886cc58ce7d2597e5eeee044539072c7") throw new Error("partial-json source changed");
const {parseStreamingJson, parseJsonWithRepair, repairJson} = await import(new URL("utils/json-parse.ts", aiRoot).pathname);
const documents = [
 ["surrogate keys", String.raw`{"\ud800":1,"\udc00":2,"\ufffd":3,"\ud800":4}`],
 ["surrogate values", String.raw`{"high":"\uD800","low":"\uDC00","pair":"\ud83d\ude00","reversed":"\udc00\ud800"}`],
 ["paired key identity", '{' + JSON.stringify("😀") + ':1,"middle":2,' + String.raw`"\ud83d\ude00":3}`],
 ["index order", String.raw`{"z":0,"10":1,"2":2,"\u0031":3,"01":4,"4294967295":5,"4294967294":6}`],
 ["duplicate key identity", String.raw`{"a":1,"b":2,"\u0061":3,"2":4,"\u0032":5}`],
 ["HTML separators", JSON.stringify({"<>&\u2028\u2029":"<>&\u2028\u2029","Tiếng Việt":"😀"})],
 ["escaped keys", String.raw`{"\n":1,"\u000a":2,"\"":3,"\\":4,"\/":5,"/":6,"":7}`],
 ["prototype keys", String.raw`{"__proto__":{"a":1},"constructor":2,"prototype":3,"toString":4,"\u005f_proto__":5}`],
 ["nested objects", String.raw`{"outer":{"\ud800":1,"\udc00":2,"2":3,"1":4},"items":[{"\ud800":"\udc00"}]}`],
 ["numbers", '{"values":[9007199254740993,1e309,-1e309,-0,1e-400,1e-7,1e21,5e-324]}'],
 ["partial constants", '{"values":[null,true,false,Infinity,-Infinity,NaN]}'],
 ["repair controls", '{"line\nkey":"line\nvalue","tab\tkey":"tab\tvalue"}'],
 ["repair escapes", String.raw`{"bad\q":"value\q","windows":"C:\path\file","last":"tail\\"}`],
 ["lenient punctuation", '{"a"=1 "b":2,"c":true false,"d":3} trailing'],
 ["malformed exponent", '{"😀":1,"value":12e+}'],
 ["incomplete unicode", '{' + JSON.stringify("😀") + String.raw`:"first","value":"tail\uD8`],
];
const cases = [];
for (const [name, document] of documents) {
 let input = "";
 for (const scalar of ["", ...document]) {
  input += scalar;
  let complete: string | undefined;
  try { complete = JSON.stringify(parseJsonWithRepair(input)); } catch {}
  cases.push({name:`${name}/${input.length}`, input, repaired:repairJson(input), complete, streaming:JSON.stringify(parseStreamingJson(input))});
 }
}
const output = JSON.stringify({upstreamCommit:manifest.commit, partialJSONSHA256, cases},null,2)+"\n";
const destination = new URL("pi-json-prefixes.json",import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination,"utf8") !== output) throw new Error("JSON prefix fixtures differ; regenerate and review");
} else writeFileSync(destination,output);
console.log(`Verified ${cases.length} JSON prefixes against Pi ${manifest.commit}`);
