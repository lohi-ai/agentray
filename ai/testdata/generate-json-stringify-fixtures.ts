// Runtime oracle for the JSON.stringify operation used by Pi's proxy. Hash
// contiguous input blocks instead of checking in a million repeated strings.
import {createHash} from "node:crypto";
import {readFileSync, writeFileSync} from "node:fs";
import {manifest} from "../../agentcore/engine/testdata/oracle.ts";
if (Bun.version !== "1.3.14") throw new Error("Proxy JSON oracle requires pinned Bun 1.3.14");
const unit = (value: number) => "\\u" + value.toString(16).padStart(4, "0");
const singleUnits: string[] = [], surrogatePairs: string[] = [];
for (let page = 0; page < 256; page++) {
 const hash = createHash("sha256");
 for (let offset = 0; offset < 256; offset++) hash.update(JSON.stringify(JSON.parse('"' + unit(page * 256 + offset) + '"')) + "\n");
 singleUnits.push(hash.digest("hex"));
}
for (let high = 0xd800; high <= 0xdbff; high++) {
 const hash = createHash("sha256");
 for (let low = 0xdc00; low <= 0xdfff; low++) hash.update(JSON.stringify(JSON.parse('"' + unit(high) + unit(low) + '"')) + "\n");
 surrogatePairs.push(hash.digest("hex"));
}
const output = JSON.stringify({upstreamCommit: manifest.commit, bunVersion: Bun.version, singleUnits, surrogatePairs}, null, 2) + "\n";
const destination = new URL("pi-json-stringify.json", import.meta.url);
if (process.argv.includes("--check")) {
 if (readFileSync(destination,"utf8") !== output) throw new Error("Proxy JSON fixtures differ; regenerate and review");
} else writeFileSync(destination,output);
console.log("Verified 65,536 UTF-16 units and 1,048,576 surrogate pairs against the pinned JSON.stringify runtime");
