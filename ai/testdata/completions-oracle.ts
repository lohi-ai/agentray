import {readFileSync} from "node:fs";
import {plugin} from "bun";
import {aiRoot,manifest} from "../../agentcore/engine/testdata/oracle.ts";
// Expose private pure helpers without modifying the pinned reference files.
// Function bodies and all their dependencies execute from the original source.
plugin({name:"completions-oracle",setup(build){build.onLoad({filter:/\/api\/openai-completions\.ts$/},args=>({loader:"ts",contents:readFileSync(args.path,"utf8")+"\nexport { getCompat as oracleCompat, convertTools as oracleTools, buildParams as oracleParams };\n"}))}});
export const {convertMessages,oracleCompat,oracleTools,oracleParams,stream,streamSimple}=await import(new URL("api/openai-completions.ts",aiRoot).pathname);
export {aiRoot,manifest};
