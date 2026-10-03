import { plugin } from "bun";
import { root, manifest } from "../../../agentcore/engine/testdata/oracle.ts";
plugin({name:"native-callback-oracle",setup(build){
 build.onResolve({filter:/^@earendil-works\/pi-agent-core$/},()=>({path:new URL("upstream/packages/agent/src/index.ts",root).pathname}));
 build.onResolve({filter:/^@earendil-works\/pi-telemetry$/},()=>({path:new URL("upstream/packages/telemetry/src/index.ts",root).pathname}));
 build.onResolve({filter:/^\.\/native-stream\.ts$/},()=>({path:"native-stream",namespace:"unused-native"}));
 build.onLoad({filter:/.*/,namespace:"unused-native"},()=>({loader:"js",contents:'export function nativeStream(...args){if(globalThis.nativeOracleStream)return globalThis.nativeOracleStream(...args);throw new Error("native provider must not run in callback oracle")}'}));
}});
export const {PiWorker}=await import(new URL("runtime/worker.ts",root).pathname);
export const {InMemoryTelemetryContext}=await import(new URL("upstream/packages/telemetry/src/index.ts",root).pathname);
export const {AssistantMessageEventStream}=await import(new URL("upstream/packages/ai/src/utils/event-stream.ts",root).pathname);
export { manifest };
