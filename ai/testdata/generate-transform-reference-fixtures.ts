import { readFileSync, writeFileSync } from "node:fs";
import { aiRoot, manifest } from "../../agentcore/engine/testdata/oracle.ts";
const { transformMessages } = await import(new URL("api/transform-messages.ts", aiRoot).pathname);
const clone = (value: any) => JSON.parse(JSON.stringify(value));
Date.now = () => 100;
const phases = ["plain", "id_equal", "id_different", "fields", "source_fields", "source_replace", "source_stop", "next_slot", "next_grow", "model_input", "model_id", "prior_message", "later_message", "later_content"];
const cases = [];
for (const signed of [false, true]) for (const vision of [false, true]) for (const phase of phases) {
  const call = (id: string) => ({ type: "toolCall", id, name: "echo", arguments: {}, ...(signed ? { thoughtSignature: "signed" } : {}) });
  const assistant = (content: any[]) => ({ role: "assistant", content, timestamp: 1, api: "test", provider: "test", model: "source", stopReason: "toolUse", usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } });
  const user = (text: string) => ({ role: "user", content: [{ type: "image", data: "a", mimeType: "image/png" }, { type: "text", text }], timestamp: 0 });
  const result = (id: string) => ({ role: "toolResult", toolCallId: id, toolName: "echo", content: [{ type: "text", text: "done" }], isError: false, timestamp: 2 });
  const messages: any[] = [user("prior"), assistant([call("call"), call("second"), { type: "text", text: "tail" }]), result("call"), result("changed"), user("later"), assistant([call("follow")]), result("follow"), { role: "system", content: null, timestamp: 3 }];
  const model = { id: "target", api: "test", provider: "test", input: vision ? ["text", "image"] : ["text"] };
  const input = clone({ name: `${signed ? "signed" : "plain"}/${vision ? "vision" : "text"}/${phase}`, phase, messages, model });
  const originalMessages = [...messages], originalBlocks = messages.flatMap((m) => m.content ?? []);
  const callbacks: any[] = [];
  const output = transformMessages(messages, model, (id: string, target: any, source: any) => {
    callbacks.push({ id, sameModel: target === model, sameSource: source === messages[1] || source === messages[5], source: clone(source), model: clone(target) });
    if (id !== "call") return id;
    const block = source.content[0];
    switch (phase) {
      case "id_equal": block.id = "changed"; return "changed";
      case "id_different": block.id = "changed"; return "normalized";
      case "fields": block.name = "edited"; block.arguments = { edited: true }; block.thoughtSignature = "changed"; break;
      case "source_fields": source.timestamp = 9; source.provider = "changed"; break;
      case "source_replace": source.content = [{ type: "text", text: "replacement" }]; break;
      case "source_stop": source.stopReason = "error"; break;
      case "next_slot": source.content[1] = { type: "text", text: "replacement" }; break;
      case "next_grow": for (let i = 0; i < 32; i++) source.content.push(call(`extra-${i}`)); break;
      case "model_input": target.input = vision ? ["text"] : ["text", "image"]; break;
      case "model_id": target.id = "source"; break;
      case "prior_message": messages[0].timestamp = 9; messages[0].content[1].text = "changed"; break;
      case "later_message": messages[4].timestamp = 9; messages[4].content[1].text = "changed"; break;
      case "later_content": messages[4].content = [{ type: "text", text: "replacement" }]; break;
    }
    return id;
  });
  cases.push({ input, expected: { messages: output, source: messages, model, callbacks, messageIndices: output.map((m: any) => originalMessages.indexOf(m)), blockIndices: output.map((m: any) => m.content.map((b: any) => originalBlocks.indexOf(b))) } });
}
const serialized = JSON.stringify({ upstreamCommit: manifest.commit, cases }, null, 2) + "\n";
const destination = new URL("pi-transform-references.json", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== serialized) throw new Error("Transform reference fixtures changed");
} else writeFileSync(destination, serialized);
console.log(`Verified ${cases.length} transform-reference cases against Pi ${manifest.commit}`);
