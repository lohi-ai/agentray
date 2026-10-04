// JSON.parse/stringify is the usage boundary used by Pi's proxy and host adapter.
import {readFileSync,writeFileSync} from "node:fs";
import {manifest} from "../../agentcore/engine/testdata/oracle.ts";
const rootFields = ["input","output","cacheRead","cacheWrite","cacheWrite1h","reasoning","totalTokens"];
const costFields = ["input","output","cacheRead","cacheWrite","total"];
const classify = (n: number) => Number.isNaN(n) ? "NaN" : n === Infinity ? "Infinity" : n === -Infinity ? "-Infinity" : Object.is(n,-0) ? "-0" : "finite";
const numbers = (usage: any) => Object.fromEntries([
  ...rootFields.filter(k => typeof usage[k] === "number").map(k => [k,classify(usage[k])]),
  ...costFields.filter(k => typeof usage.cost?.[k] === "number").map(k => [`cost.${k}`,classify(usage.cost[k])]),
]);
const sources = [
  '{}',
  '{"input":null,"output":null,"reasoning":null,"cacheWrite1h":null,"cost":{"input":null}}',
  '{"cost":null}',
  '{"input":1e400,"output":-1e400,"cacheRead":1e400,"cacheWrite":-1e400,"cacheWrite1h":1e400,"reasoning":-1e400,"totalTokens":1e400,"cost":{"input":1e400,"output":-1e400,"cacheRead":1e400,"cacheWrite":-1e400,"total":1e400}}',
  '{"input":-0,"output":-0,"reasoning":-0,"cacheWrite1h":-0,"cost":{"total":-0}}',
  '{"input":1e-9999,"output":-1e-9999,"cost":{"total":-1e-9999}}',
  '{"input":9007199254740993,"cost":{"total":18446744073709551615},"vendor":[9007199254740993,1e400,-0]}',
  '{"input":3,"cost":{"input":0.1,"currency":"USD","overflow":1e400},"providerMeter":{"requests":9007199254740993,"values":[-0,-1e400]}}',
];
const cases=[];
for (const raw of sources) for (const mutation of ["none","NaN","Infinity","-Infinity","-0","42"]) {
  const value=JSON.parse(raw);
  const before=JSON.parse(JSON.stringify(value)), beforeNumbers=numbers(value);
  if(mutation!=="none") {
    const n=Number(mutation);
    value.input=n;value.reasoning=n;value.cacheWrite1h=n;
    value.cost??={};value.cost.total=n;
  }
  cases.push({input:{raw,mutation},expected:{before,beforeNumbers,after:JSON.parse(JSON.stringify(value)),afterNumbers:numbers(value)}});
}
const output=JSON.stringify({upstreamCommit:manifest.commit,cases},null,2)+"\n";
const destination=new URL("pi-usage-json.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("Pi usage JSON fixtures differ");}
else writeFileSync(destination,output);
console.log(`Verified ${cases.length} usage JSON cases against Pi ${manifest.commit}`);
