import type { StreamFn } from "@earendil-works/pi-agent-core";
import { lazyStream, type KnownApi } from "@earendil-works/pi-ai";

// Explicit imports keep every executable provider implementation in the pinned
// source tree. No generated catalog, global registry, or Go message projection.
const implementations = {
  "openai-completions": () => import("../upstream/packages/ai/src/api/openai-completions.ts"),
  "openai-responses": () => import("../upstream/packages/ai/src/api/openai-responses.ts"),
  "azure-openai-responses": () => import("../upstream/packages/ai/src/api/azure-openai-responses.ts"),
  "openai-codex-responses": () => import("../upstream/packages/ai/src/api/openai-codex-responses.ts"),
  "anthropic-messages": () => import("../upstream/packages/ai/src/api/anthropic-messages.ts"),
  "bedrock-converse-stream": () => import("../upstream/packages/ai/src/api/bedrock-converse-stream.ts"),
  "google-generative-ai": () => import("../upstream/packages/ai/src/api/google-generative-ai.ts"),
  "google-vertex": () => import("../upstream/packages/ai/src/api/google-vertex.ts"),
  "mistral-conversations": () => import("../upstream/packages/ai/src/api/mistral-conversations.ts"),
  "pi-messages": () => import("../upstream/packages/ai/src/api/pi-messages.ts"),
} satisfies Record<KnownApi, () => Promise<unknown>>;

export function nativeStream(...[model, context, options]: Parameters<StreamFn>) {
  return lazyStream(model, async () => {
    if (!Object.hasOwn(implementations, model.api)) throw new Error(`Unsupported native Pi API: ${model.api}`);
    const implementation = await implementations[model.api as KnownApi]();
    return (implementation.streamSimple as StreamFn)(model, context, options);
  });
}
