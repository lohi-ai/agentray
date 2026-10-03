// Development oracle only. The Go package and its tests do not invoke Bun.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { InMemoryTelemetryContext, type TelemetrySpan, type TelemetryContext } from "../../third_party/pi/upstream/packages/telemetry/src/index.ts";

execFileSync("python3", [new URL("../../third_party/pi/verify.py", import.meta.url).pathname], { stdio: "inherit" });
const manifest = JSON.parse(readFileSync(new URL("../../third_party/pi/UPSTREAM.json", import.meta.url), "utf8"));
const destination = new URL("pi-spans.json", import.meta.url);

type Action = {
  op: "span" | "attributes" | "event" | "status" | "snapshot" | "failure";
  name?: string; attributes?: Record<string, any>; undefinedAttributes?: string[]; status?: any;
  actions?: Action[]; inspection?: Action[]; target?: string; message?: string;
};
const span = (name: string, actions: Action[] = [], attributes?: Record<string, any>): Action => ({ op: "span", name, actions, ...(attributes ? { attributes } : {}) });
const cases: { name: string; actions: Action[] }[] = [
  { name: "empty", actions: [] },
  { name: "success and active snapshot", actions: [span("success", [{ op: "snapshot" }])] },
  { name: "nested parentage and end order", actions: [span("parent", [span("first"), span("second", [span("grandchild")])]), span("another-root")] },
  { name: "attribute merges and ordered events", actions: [{ ...span("recording", [
    { op: "attributes", attributes: { count: 1, overwrite: "middle" } },
    { op: "attributes", attributes: { overwrite: "end" }, undefinedAttributes: ["count"] },
    { op: "event", name: "first", attributes: { index: 1 }, undefinedAttributes: ["ignored"] },
    { op: "event", name: "second", attributes: { index: 2 } },
  ], { start: "value", overwrite: "start" }), undefinedAttributes: ["ignored"] }] },
  { name: "array and scalar attributes", actions: [span("arrays", [
    { op: "event", name: "all", attributes: { strings: ["a", "b"], numbers: [1, 2.5], bools: [false, true], empty: [] } },
  ], { text: "xin chào", number: 3.5, flag: false, strings: ["initial"], numbers: [1], bools: [true] })] },
  { name: "automatic error", actions: [span("failed", [{ op: "failure", message: "failed callback" }])] },
  { name: "automatic error overwrites status from error inspection", actions: [span("failed", [{ op: "failure", message: "original failure", inspection: [
    { op: "status", status: { status: "ok" } }, { op: "snapshot" },
  ] }])] },
  { name: "automatic error overwrites detailed status from error inspection", actions: [span("failed", [{ op: "failure", message: "original failure", inspection: [
    { op: "status", status: { status: "error", error: { name: "Inspection", message: "intermediate" } } }, { op: "snapshot" },
  ] }])] },
  { name: "error inspection retains attributes and events before settlement", actions: [span("failed", [{ op: "failure", message: "original failure", inspection: [
    { op: "attributes", attributes: { inspected: true } }, { op: "event", name: "inspect", attributes: { phase: "message" } }, { op: "snapshot" },
  ] }])] },
  { name: "unreadable error overwrites status from inspection without details", actions: [span("failed", [{ op: "failure", message: "original failure", inspection: [
    { op: "status", status: { status: "ok" } }, { op: "snapshot" }, { op: "failure", message: "unreadable" },
  ] }])] },
  { name: "preexisting explicit status suppresses error inspection", actions: [span("failed", [
    { op: "status", status: { status: "error", error: { name: "Explicit", message: "retained" } } },
    { op: "failure", message: "original failure", inspection: [{ op: "event", name: "must not inspect" }, { op: "failure", message: "unreadable" }] },
  ])] },
  { name: "last explicit status wins", actions: [span("explicit", [
    { op: "status", status: { status: "error", error: { name: "Expected", message: "first" } } },
    { op: "status", status: { status: "ok" } },
    { op: "failure", message: "does not overwrite" },
  ])] },
  { name: "explicit error survives rejection", actions: [span("explicit", [
    { op: "status", status: { status: "error", error: { name: "Expected", message: "retained" } } },
    { op: "failure", message: "ignored" },
  ])] },
  { name: "error status without details", actions: [span("explicit", [{ op: "status", status: { status: "error" } }])] },
  { name: "settled span inert but child callback admitted", actions: [span("settled", [], { value: "initial" }),
    { op: "attributes", target: "settled", attributes: { value: "late" } },
    { op: "event", target: "settled", name: "late" },
    { op: "status", target: "settled", status: { status: "error" } },
    { ...span("late-child", [{ op: "snapshot" }, span("late-grandchild")]), target: "settled" },
  ] },
];

for (const [name, status] of [
  ["missing status", {}],
  ["empty status", {status: ""}],
  ["unknown status", {status: "invalid"}],
  ["case sensitive status", {status: "OK"}],
  ["unknown status with details", {status: "pending", error: {name: "Explicit", message: "kept"}}],
  ["ok ignores details", {status: "ok", error: {name: "Ignored", message: "ignored"}}],
  ["empty error details", {status: "error", error: {name: "", message: ""}}],
  ["null error details", {status: "error", error: null}],
] as [string, any][]) {
  for (const failure of ["none", "ordinary", "unreadable"]) {
    const actions: Action[] = [{op: "status", status: {status: "ok"}}, {op: "status", status}, {op: "snapshot"}];
    if (failure !== "none") actions.push({op: "failure", message: "callback failed", ...(failure === "unreadable" ? {inspection: [{op: "event", name: "must not inspect"}, {op: "failure", message: "unreadable"}] as Action[]} : {})});
    cases.push({name: `status normalization/${name}/${failure}`, actions: [span("explicit", actions)]});
  }
}

const results = [];
for (const input of cases) {
  const recorder = new InMemoryTelemetryContext();
  const retained = new Map<string, TelemetrySpan>();
  const snapshots: unknown[] = [];
  const failures: string[] = [];
  const admitted: string[] = [];
  const inspectedFailures = new WeakMap<Error, string>();
  const attributes = (action: Action) => ({...action.attributes, ...Object.fromEntries((action.undefinedAttributes ?? []).map(key => [key, undefined]))});
  async function run(actions: Action[], context: TelemetryContext): Promise<void> {
    for (const action of actions) {
      const target = action.target ? retained.get(action.target)! : context;
      switch (action.op) {
        case "span":
          try {
            await target.startSpan({ name: action.name!, attributes: attributes(action) }, async child => {
              admitted.push(action.name!);
              retained.set(action.name!, child);
              await run(action.actions ?? [], child);
            });
          } catch (error) { failures.push(inspectedFailures.get(error as Error) ?? (error as Error).message); }
          break;
        case "attributes": (target as TelemetrySpan).setAttributes(attributes(action)); break;
        case "event": (target as TelemetrySpan).addEvent(action.name!, attributes(action)); break;
        case "status": (target as TelemetrySpan).setStatus(action.status); break;
        case "snapshot": snapshots.push(recorder.getSpans()); break;
        case "failure": {
          const failure = new Error(action.message);
          if (action.inspection) {
            inspectedFailures.set(failure, action.message!);
            Object.defineProperty(failure, "message", { get() {
              for (const step of action.inspection!) {
                switch (step.op) {
                  case "attributes": (target as TelemetrySpan).setAttributes(attributes(step)); break;
                  case "event": (target as TelemetrySpan).addEvent(step.name!, attributes(step)); break;
                  case "status": (target as TelemetrySpan).setStatus(step.status); break;
                  case "snapshot": snapshots.push(recorder.getSpans()); break;
                  case "failure": throw new Error(step.message);
                  default: throw new Error(`unsupported synchronous inspection ${step.op}`);
                }
              }
              return action.message;
            } });
          }
          throw failure;
        }
      }
    }
  }
  await run(input.actions, recorder);
  results.push({ ...input, expected: { spans: recorder.getSpans(), snapshots, failures, admitted } });
}
const thrownValues = [];
for (const input of [
  { kind: "value", value: null },
  { kind: "value", value: false },
  { kind: "value", value: 0 },
  { kind: "value", value: "" },
  { kind: "value", value: ["failure", null] },
  { kind: "value", value: { name: "Error", message: "plain object" } },
  { kind: "error", name: "Error", message: "ordinary failure" },
  { kind: "error", name: "CustomError", message: "named failure" },
]) {
  for (const explicitStatus of [undefined, { status: "ok" }, { status: "error", error: { name: "Explicit", message: "retained" } }]) {
    const recorder = new InMemoryTelemetryContext();
    const failure = input.kind === "error" ? Object.assign(new Error(input.message), { name: input.name }) : input.value;
    let sameFailure = false;
    try {
      await recorder.startSpan({ name: "failure" }, span => {
        if (explicitStatus) span.setStatus(explicitStatus as any);
        throw failure;
      });
    } catch (error) { sameFailure = Object.is(error, failure); }
    thrownValues.push({ input: { ...input, explicitStatus }, expected: { spans: recorder.getSpans(), sameFailure } });
  }
}
const output = JSON.stringify({ upstreamCommit: manifest.commit, cases: results, thrownValues }, null, 2) + "\n";
if (process.argv.includes("--check")) {
  if (readFileSync(destination, "utf8") !== output) throw new Error("Pi telemetry fixtures differ; regenerate and review");
} else writeFileSync(destination, output);
console.log(`Verified ${results.length} telemetry cases and ${thrownValues.length} thrown-value cases against Pi ${manifest.commit}`);
