import { Agent, type AgentOptions, type AgentTool, type StreamFn } from "@earendil-works/pi-agent-core";
import { createAssistantMessageEventStream, type AssistantMessage, type AssistantMessageEvent } from "@earendil-works/pi-ai";
import { InMemoryTelemetryContext, NOOP_TELEMETRY_CONTEXT, type TelemetryContext } from "@earendil-works/pi-telemetry";
import { nativeStream } from "./native-stream.ts";
import { prepareAskArguments } from "../../../agentcore/plugins/ask/pi.mts";

declare const PI_UPSTREAM_COMMIT: string;

type WireError = { name: string; message: string };
type Pending = {
  resolve(value: any): void;
  reject(error: unknown): void;
  progress?: (value: any) => void;
  cleanup(): void;
};

function wireError(error: unknown): WireError {
  return error instanceof Error
    ? { name: error.name, message: error.message }
    : { name: "Error", message: String(error) };
}

// One process owns one Agent. This adapter only transports the public API;
// scheduling, queues, argument validation, event order, and state live in Pi.
export class PiWorker {
  private agent?: Agent;
  private pending = new Map<string, Pending>();
  private nextID = 0;
  private telemetry = new InMemoryTelemetryContext();
  private activeTelemetry: TelemetryContext = NOOP_TELEMETRY_CONTEXT;
  private closed = false;
  private activeCall?: string;
  private traceRequests = false;
  private nextRequestID = 0;
  private pendingTraces = new Set<Promise<void>>();
  private traceTail: Promise<void> = Promise.resolve();

  private readonly send: (value: unknown) => void;

  constructor(send: (value: unknown) => void) { this.send = send; }

  private rpc(method: string, params: unknown, signal?: AbortSignal, progress?: (value: any) => void): Promise<any> {
    if (this.closed) return Promise.reject(new Error("Pi transport closed"));
    if (signal?.aborted) return Promise.reject(signal.reason);
    const id = `callback-${++this.nextID}`;
    return new Promise((resolve, reject) => {
      const abort = () => {
        this.pending.delete(id);
        signal?.removeEventListener("abort", abort);
        this.send({ kind: "cancel", id });
        reject(signal?.reason);
      };
      const cleanup = () => signal?.removeEventListener("abort", abort);
      this.pending.set(id, { resolve, reject, progress, cleanup });
      signal?.addEventListener("abort", abort, { once: true });
      try {
        this.send({ kind: "callback", id, method, params });
      } catch (error) {
        this.pending.delete(id);
        cleanup();
        reject(error);
      }
    });
  }

  private tools(definitions: any[]): AgentTool[] {
    for (const definition of definitions) {
      if (definition.prepareArguments) throw new Error("prepareArguments requires the native TypeScript API");
      if (definition.agentrayPrepareArguments !== undefined && definition.agentrayPrepareArguments !== "ask-v1") {
        throw new Error(`Unknown native argument preparer: ${definition.agentrayPrepareArguments}`);
      }
    }
    return definitions.map(({ agentrayPrepareArguments, ...definition }) => ({
      ...definition,
      ...(agentrayPrepareArguments === "ask-v1" ? { prepareArguments: prepareAskArguments } : {}),
      execute: (toolCallId, args, signal, onUpdate) => this.activeTelemetry.startSpan(
        { name: "agentray.tool.execute", attributes: { "tool.name": definition.name } },
        async (span) => {
          const result = await this.rpc("tool", { toolCallId, toolName: definition.name, args }, signal, onUpdate);
          if (result?.isError) span.setStatus({ status: "error" });
          return result;
        },
      ),
    }));
  }

  private observeRequest(model: any, context: any, run: Parameters<TelemetryContext["startSpan"]>[1]): Promise<AssistantMessage> {
    const requestID = ++this.nextRequestID;
    const startedAtMs = Date.now();
    let requestContext: unknown;
    if (this.traceRequests) {
      try { requestContext = JSON.parse(JSON.stringify(context)); } catch { /* passive observer */ }
    }
    const result = this.activeTelemetry.startSpan({ name: "agentray.ai.request", attributes: {
      "model.id": model.id, "agentray.request.id": requestID,
    } }, run) as Promise<AssistantMessage>;
    if (this.traceRequests) {
      const record = async (response?: AssistantMessage, error?: unknown) => {
        try {
          const spans = this.telemetry.getSpans();
          const root = spans.find((span) => span.name === "agentray.ai.request" && span.attributes["agentray.request.id"] === requestID);
          const ids = new Set<number>();
          const requestSpans = spans.filter((span) => {
            if (span.id === root?.id || (span.parentId !== null && ids.has(span.parentId))) { ids.add(span.id); return true; }
            return false;
          });
          // Stream options include credentials and live callbacks; they are not
          // trace data. The native request context and response remain intact.
          const packet = { requestID, startedAtMs, durationMs: Date.now() - startedAtMs,
            upstreamCommit: PI_UPSTREAM_COMMIT, model, context: requestContext, response,
            ...(error === undefined ? {} : { error: wireError(error) }), spans: requestSpans,
          };
          // Leave headroom below the Go bridge's 64 MiB frame limit.
          if (Buffer.byteLength(JSON.stringify(packet), "utf8") > 60 * 1024 * 1024) return;
          this.traceTail = this.traceTail.then(async () => {
            try { await this.rpc("trace", packet, AbortSignal.timeout(5000)); } catch { /* passive observer */ }
          });
          await this.traceTail;
        } catch { /* tracing must never change the provider outcome */ }
      };
      const trace = result.then((response) => record(response), (error) => record(undefined, error));
      this.pendingTraces.add(trace);
      void trace.finally(() => this.pendingTraces.delete(trace));
    }
    return result;
  }

  private async flushTraces(): Promise<void> {
    while (this.pendingTraces.size) await Promise.all([...this.pendingTraces]);
  }

  private stream: StreamFn = (model, context, options) => {
    const stream = createAssistantMessageEventStream();
    let started = false;
    let terminalMessage: AssistantMessage | undefined;
    let failure: unknown;
    let failed = false;
    const { signal, telemetryContext: _telemetry, onPayload: _payload, onResponse: _response,
      onProviderStreamEvent: _providerEvent, ...wireOptions } = options ?? {};
    const forward = (event: AssistantMessageEvent) => {
      if (terminalMessage) return;
      started = true;
      if (event.type === "done") terminalMessage = event.message;
      if (event.type === "error") terminalMessage = event.error;
      stream.push(event);
    };
    const result = this.observeRequest(model, context, async (span) => {
      try {
        const response: AssistantMessage = await this.rpc("stream", { model, context, options: wireOptions }, signal, forward);
        const message = terminalMessage ?? response;
        if (!message || message.role !== "assistant") throw new Error("stream callback must return an assistant message");
        if (message.stopReason === "pending") throw new Error("stream callback returned an unfinished assistant message");
        if (!terminalMessage) {
          if (!started) forward({ type: "start", partial: message });
          if (message.stopReason === "error" || message.stopReason === "aborted") {
            forward({ type: "error", reason: message.stopReason, error: message });
          } else {
            forward({ type: "done", reason: message.stopReason, message });
          }
        }
        if (message.stopReason === "error" || message.stopReason === "aborted") span.setStatus({ status: "error" });
        return message;
      } catch (error) {
        // Let the original Agent synthesize its own failure lifecycle. A
        // transport adapter must not invent a different assistant message.
        failed = true;
        failure = error;
        stream.end();
        throw error;
      }
    });
    // The loop starts reading before the callback settles. Observe rejection
    // immediately, then deliver the same failure through iteration/result().
    void result.catch(() => {});
    const iterate = stream[Symbol.asyncIterator].bind(stream);
    stream[Symbol.asyncIterator] = async function* () {
      const iterator = iterate();
      try {
        while (true) {
          const next = await iterator.next();
          if (next.done) break;
          yield next.value;
        }
      } finally {
        await iterator.return?.();
      }
      if (failed) throw failure;
    };
    stream.result = () => result;
    return stream;
  };

  private nativeStream: StreamFn = (model, context, options) => {
    let stream: ReturnType<typeof nativeStream> | undefined;
    const result = this.observeRequest(model, context, async (span) => {
      stream = nativeStream(model, context, { ...options, telemetryContext: span });
      const message = await stream.result();
      if (message.stopReason === "error" || message.stopReason === "aborted") span.setStatus({ status: "error" });
      return message;
    });
    // Pi telemetry admits callbacks synchronously. Observe the result without
    // a second queue/iterator: native partial messages are mutable, so buffering
    // them here would change the intermediate frames seen by the Agent.
    void result.catch(() => {});
    if (!stream) throw new Error("Telemetry did not synchronously admit native stream");
    return stream;
  };

  private initialize(params: any) {
    if (this.agent) throw new Error("Agent already initialized");
    const { callbacks = [], initialState, streamMode = "callback", streamOptions = {}, traceRequests = false, ...options } = params ?? {};
    if (streamMode !== "callback" && streamMode !== "native") throw new Error(`Unknown stream mode: ${streamMode}`);
    if (!streamOptions || typeof streamOptions !== "object" || Array.isArray(streamOptions)) throw new Error("streamOptions must be an object");
    for (const name of ["signal", "telemetryContext", "apiKey", "fetch", "onPayload", "onResponse", "onProviderStreamEvent"]) {
      if (Object.hasOwn(streamOptions, name)) throw new Error(`Stream option ${name} belongs to the native runtime or its callback`);
    }
    this.traceRequests = traceRequests === true;
    const streamFn = streamMode === "native" ? this.nativeStream : this.stream;
    const config: AgentOptions = {
      ...options,
      initialState: initialState ? { ...initialState, tools: this.tools(initialState.tools ?? []) } : undefined,
      streamFn: (model, context, requestOptions) => streamFn(model, context, { ...streamOptions, ...requestOptions }),
    };
    const hooks = new Set([
      "convertToLlm", "transformContext", "getApiKey", "beforeToolCall", "afterToolCall",
      "finishTurn", "prepareRequest", "prepareNextTurn", "prepareNextTurnWithContext",
    ]);
    for (const name of callbacks) {
      if (name === "onPayload") {
        config.onPayload = (payload, model) => this.rpc(name, { payload, model }, this.agent?.signal);
        continue;
      }
      if (!hooks.has(name)) throw new Error(`Unsupported callback: ${name}`);
      (config as any)[name] = async (value: unknown, signal?: AbortSignal) => {
        const result = await this.rpc(name, name === "prepareNextTurn" ? null : value,
          name === "prepareNextTurn" ? value as AbortSignal | undefined
            : signal ?? (name === "getApiKey" ? this.agent?.signal : undefined));
        if ((name === "prepareRequest" || name === "prepareNextTurn" || name === "prepareNextTurnWithContext") && result?.context?.tools) {
          result.context.tools = this.tools(result.context.tools);
        }
        return result;
      };
    }
    this.agent = new Agent(config);
    this.agent.subscribe((event) => this.rpc("event", event));
    return { protocol: 1, upstreamCommit: PI_UPSTREAM_COMMIT };
  }

  private async dispatch(method: string, params: any, id: string): Promise<unknown> {
    if (method === "initialize") return this.initialize(params);
    const agent = this.agent;
    if (!agent) throw new Error("Agent not initialized");
    switch (method) {
      case "prompt":
      case "continue": {
        // Let Pi reject concurrent runs with its own public error contract.
        if (agent.state.isStreaming) {
          return method === "prompt" ? agent.prompt(params.input, params.images) : agent.continue();
        }
        this.activeCall = id;
        return this.telemetry.startSpan({ name: "agentray.agent.run" }, async (span) => {
          this.activeTelemetry = span;
          try {
            if (method === "prompt") await agent.prompt(params.input, params.images);
            else await agent.continue();
            if (agent.state.errorMessage) span.setStatus({ status: "error" });
          } finally {
            await this.flushTraces();
            this.activeTelemetry = NOOP_TELEMETRY_CONTEXT;
            this.activeCall = undefined;
          }
        });
      }
      case "state": return { ...agent.state, pendingToolCalls: [...agent.state.pendingToolCalls] };
      case "setState": {
        const allowed = new Set(["model", "thinkingLevel", "messages", "tools"]);
        for (const key of Object.keys(params)) if (!allowed.has(key)) throw new Error(`State field is read-only: ${key}`);
        const values = { ...params };
        if (values.tools) values.tools = this.tools(values.tools);
        for (const [key, value] of Object.entries(values)) (agent.state as any)[key] = value;
        return;
      }
      case "configure": {
        const allowed = new Set(["steeringMode", "followUpMode", "sessionId", "thinkingBudgets", "transport", "maxRetryDelayMs", "toolExecution"]);
        for (const key of Object.keys(params)) if (!allowed.has(key)) throw new Error(`Unknown setting: ${key}`);
        for (const [key, value] of Object.entries(params)) (agent as any)[key] = value;
        return;
      }
      case "steer": return agent.steer(params);
      case "followUp": return agent.followUp(params);
      case "clearSteeringQueue": return agent.clearSteeringQueue();
      case "clearFollowUpQueue": return agent.clearFollowUpQueue();
      case "clearAllQueues": return agent.clearAllQueues();
      case "hasQueuedMessages": return agent.hasQueuedMessages();
      case "peekQueuedMessages": return agent.peekQueuedMessages();
      case "abort": return agent.abort();
      case "waitForIdle": await agent.waitForIdle(); await this.flushTraces(); return;
      case "reset": return agent.reset();
      case "telemetry": return this.telemetry.getSpans();
      default: throw new Error(`Unknown method: ${method}`);
    }
  }

  receive(message: any): void {
    if (this.closed) return;
    if (message.kind === "cancelCall") {
      if (this.activeCall === message.id) this.agent?.abort();
      return;
    }
    if (message.kind === "result" || message.kind === "progress") {
      const pending = this.pending.get(message.id);
      if (!pending) return; // A late reply after cancellation is inert.
      if (message.kind === "progress") {
        pending.progress?.(message.value);
        return;
      }
      this.pending.delete(message.id);
      pending.cleanup();
      if (message.error) pending.reject(Object.assign(new Error(message.error.message), { name: message.error.name }));
      else pending.resolve(message.value);
      return;
    }
    if (message.kind !== "call" || typeof message.id !== "string") return;
    void this.dispatch(message.method, message.params, message.id).then(
      (value) => this.send({ kind: "result", id: message.id, value }),
      (error) => this.send({ kind: "result", id: message.id, error: wireError(error) }),
    );
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.agent?.abort();
    for (const pending of this.pending.values()) {
      pending.cleanup();
      pending.reject(new Error("Pi transport closed"));
    }
    this.pending.clear();
  }
}
