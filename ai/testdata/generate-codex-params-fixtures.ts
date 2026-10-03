import { readFileSync, writeFileSync } from "node:fs";
import { plugin } from "bun";
import { aiRoot, manifest } from "../../agentcore/engine/testdata/oracle.ts";
plugin({ name: "codex-params-oracle", setup(build) {
  build.onLoad({ filter: /\/api\/openai-codex-responses\.ts$/ }, args => ({ loader: "ts", contents: readFileSync(args.path, "utf8") + "\nexport { buildRequestBody as oracleParams };\n" }));
}});
const { oracleParams } = await import(new URL("api/openai-codex-responses.ts", aiRoot).pathname);
const { normalizeContext } = await import(new URL("utils/transcript.ts", aiRoot).pathname);
Date.now = () => 100;
const model = { id: "gpt-5.5", name: "Test", api: "openai-codex-responses", provider: "openai-codex", baseUrl: "https://chatgpt.com/backend-api", reasoning: true, input: ["text", "image"], maxTokens: 20000, contextWindow: 100000, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } };
const user = (content: any) => ({ role: "user", content, timestamp: 100 });
const tool = { name: "tool", description: "Tool", parameters: { type: "object", properties: { code: { type: "string" } }, required: ["code"] } };
const context = { systemPrompt: "system", messages: [user("hello")], tools: [tool] };
const inputs: any[] = [
  { name: "default" },
  { name: "no instructions or tools", context: { messages: [user("hello")] } },
  { name: "empty instructions", context: { systemPrompt: "", messages: [] } },
  { name: "section instructions", context: { messages: [{ role: "system", content: "base", sections: { a: "first", b: null, c: "second" }, timestamp: 1 }, user("hello")] } },
  { name: "empty cache key", cacheSessionId: "" },
  { name: "cache passed through unclamped", cacheSessionId: "😀".repeat(70), options: { sessionId: "different", cacheRetention: "none" } },
  { name: "sampling options not spread", model: { samplingParams: { model: "other", stream: false } }, options: { maxTokens: 50, samplingParams: { instructions: "other" }, parallelToolCalls: false } },
  ...["low", "medium", "high", "", null].map(textVerbosity => ({ name: `verbosity ${textVerbosity}`, options: { textVerbosity } })),
  ...["auto", "none", "required", null, ""].map(toolChoice => ({ name: `tool choice ${toolChoice}`, options: { toolChoice } })),
  ...[0, 0.5, null].map(temperature => ({ name: `temperature ${temperature}`, options: { temperature } })),
  ...["default", "flex", "priority", null].map(serviceTier => ({ name: `service ${serviceTier}`, options: { serviceTier } })),
  ...[true, false].flatMap(reasoning => [undefined, "none", "low", "high", null].flatMap(reasoningEffort => [{}, { off: null, high: null }, { off: "disabled", high: "mapped" }].map((thinkingLevelMap, i) => ({ name: `reasoning ${reasoning} effort ${reasoningEffort} map ${i}`, model: { reasoning, thinkingLevelMap }, options: { reasoningEffort } })))),
  ...["auto", "off", "on", "", null].map(reasoningSummary => ({ name: `summary ${reasoningSummary}`, options: { reasoningEffort: "high", reasoningSummary } })),
  ...[undefined, true, false, null].map(supportsStrictMode => ({ name: `strict ${supportsStrictMode}`, compat: { supportsStrictMode }, context: { tools: [{ ...tool, constrainedSampling: { type: "json_schema", strict: "require" } }], messages: [] } })),
  ...[true, false].map(supportsOpenAIGrammarTools => ({ name: `grammar ${supportsOpenAIGrammarTools}`, compat: { supportsOpenAIGrammarTools }, context: { tools: [{ ...tool, constrainedSampling: { type: "grammar", variants: { openai_regex: ".*" } } }], messages: [user("run")] } })),
  ...[{}, { supportsMidConvoSystemMessages: true, supportsAdditionalTools: true }, { supportsMidConvoSystemMessages: true, supportsToolSearch: true }].map((compat, i) => ({ name: `mid conversation tools ${i}`, compat, context: { messages: [{ role: "system", content: "system", timestamp: 1 }, user("run"), { role: "system", content: "new rule", timestamp: 2, toolsAdded: [tool] }, user("next")] } })),
];
const cases = [];
for (const input of inputs) {
  const chosen = { ...model, ...input.model, compat: input.compat };
  const transcript = normalizeContext(input.context ?? context);
  const before = JSON.stringify({ chosen, transcript, input });
  let params: any = null, error: string | null = null;
  try { params = oracleParams(chosen, transcript, input.options, input.cacheSessionId); } catch (failure) { error = (failure as Error).message; }
  if (JSON.stringify({ chosen, transcript, input }) !== before) throw new Error("builder mutated inputs");
  cases.push({ input, expected: { params, error } });
}
const file = new URL("pi-codex-params.json", import.meta.url);
const output = JSON.stringify({ upstreamCommit: manifest.commit, model, context, cases }, null, 2) + "\n";
if (process.argv.includes("--check")) { if (readFileSync(file, "utf8") !== output) throw new Error("Codex request oracle changed"); } else writeFileSync(file, output);
console.log(`Verified ${cases.length} Codex request-body cases against Pi ${manifest.commit}`);
