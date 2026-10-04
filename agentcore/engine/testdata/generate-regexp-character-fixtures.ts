// Development-only RegExp character-class and boundary oracle.
import {readFileSync, writeFileSync} from "node:fs";
import {manifest} from "./oracle.ts";
if (Bun.version !== "1.3.14" || process.versions.unicode !== "15.1") throw new Error("Review the pinned RegExp oracle before changing runtime");
const points = new Set(Array.from({length:128}, (_, i)=>i));
for (const code of [0x85,0xa0,0x130,0x131,0x17f,0x301,0x3b1,0x660,0x1680,0x180e,0x2000,0x200a,0x200b,0x200c,0x200d,0x2028,0x2029,0x202f,0x205f,0x2060,0x212a,0x3000,0x96ea,0xfeff,0x1f600,0x2ebf0]) {
  for (const adjacent of [code-1,code,code+1]) points.add(adjacent);
}
const inputs = ["", ...[...points].sort((a,b)=>a-b).map(code=>String.fromCodePoint(code))];
for (const text of ["a", "é", "İ", "ı", "ſ", "K", "雪", "\u0301", "😀", " ", "_", "0"]) {
  inputs.push(`a${text}`, `${text}a`, ` ${text}`, `${text} `, `${text}${text}`);
}
const patterns = [
  ...["\\d", "\\D", "\\s", "\\S", "\\w", "\\W", "[\\w]", "[^\\w]", "[\\W]", "[^\\W]", "[\\w\\W]", "[^\\w\\W]", "[\\w#]", "[^\\w#]", "[\\w^]", "[\\w-]", "[\\w\\d]", "[\\w\\p{ASCII_Hex_Digit}]", "[\\w\\u005e]"].flatMap(atom=>[`^${atom}$`, `^(?i:${atom})$`]),
  ...["\\b", "\\B"].flatMap(boundary=>[boundary, `^${boundary}.$`, `^.${boundary}$`, `^.${boundary}.$`, `(?i:${boundary})`, `^(?i:${boundary}.)$`, `^(?i:.${boundary})$`, `^(?i:.${boundary}.)$`]),
  "^(?i:\\w(?-i:\\w))$", "^(?i:(?-i:\\w)\\w)$", "^(?i:\\w)(?-i:\\w)$", "^(?i:[\\w])(?-i:[\\w])$",
  "(?<=^\\b.)$", "(?<=^.(?i:\\b))$", "(?<=^(?i:[^\\w#]))$", "(?<=^(?i:[\\w#]))$",
  "^(?i:([\\w]?))*\\1$", "^(?i:(\\w?)*)\\1$", "^(?i:(\\B|\\b))*$",
  "[\\b]", "[\\b\\w]", "[\\B]", "[a-\\w]", "[\\w-a]", "(?i:^[\\w^]*$)",
];
const cases = patterns.map(pattern=>{
  let compiled: RegExp;
  try { compiled = new RegExp(pattern, "u"); } catch { return {pattern, valid:false, accepted:[]}; }
  return {pattern, valid:true, accepted:inputs.flatMap((value,i)=>compiled.test(value)?[i]:[])};
});
// Enumerate the complete word sets independently of the sampled boundary
// corpus; the native test checks every representable Unicode scalar.
const wordSets = [false,true].map(ignoreCase=>{
  const pattern = new RegExp(ignoreCase ? "^(?i:\\w)$" : "^\\w$", "u");
  const codepoints:number[] = [];
  for(let code=0;code<=0x10ffff;code++) if(pattern.test(String.fromCodePoint(code))) codepoints.push(code);
  return {ignoreCase,codepoints};
});
const output = JSON.stringify({upstreamCommit:manifest.commit,bun:Bun.version,unicode:process.versions.unicode,inputs,cases,wordSets},null,2)+"\n";
const destination = new URL("pi-regexp-characters.json",import.meta.url);
if(process.argv.includes("--check")){if(readFileSync(destination,"utf8")!==output)throw new Error("RegExp character fixtures changed");}else writeFileSync(destination,output);
console.log(`Verified ${cases.length} patterns across ${inputs.length} inputs against Pi ${manifest.commit}`);
