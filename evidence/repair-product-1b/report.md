# Authorization repair 2 — bs-ieppk70m

STATUS: DONE
ROOT_CAUSE: authProject reloaded ambient owner membership after selecting a restricted Bearer, and chat/answer/conversation derived ReadOnly solely from that membership while ignoring the write guard.

Starting HEAD: 1af0e37d412553b1779b0c2c37045b282b5aaca9.
Task: task_d71bdefae78f; dispatch: ctx_db11389b4cb9.

## Reproduced before editing

The reviewer's original overlay ran against the starting HEAD and exited 1. The owner-only control passed; owner cookie plus analytics:read,sources:read Bearer failed with role=owner, guard_read_only=true, chat_ReadOnly=false.

- Original source: reviewer-before-probe.go.txt (verbatim review overlay).
- Portable overlay mapping: reviewer-before-overlay.json.
- Executed failure: failing-before-mixed-credential.log.
- After probe: reviewer-after-probe.go.txt and after-reviewer-probe.log. The only probe changes bind the retained auth context and observe agentRunReadOnly instead of the superseded inline ReadOnly expression; inputs and assertions are identical.

## Repair

authProject retains the selected opcore.Principal independently of the session user and membership used to own conversation history. It reuses principalAndProject for credential precedence/capture refusal and reapplies projectForPrincipal after membership loading so a management credential cannot regain the capture key.

Chat, parked answers, and durable conversation turns all set ReadOnly through agentRunReadOnly, which combines the guard's read-only decision with the selected principal's dashboards:write authority. Restricted credentials stay read-only even without middleware; an explicitly authorized author retains configured tools. The installed Data Analyst and portable Lohi skill remain intact.

Regression coverage pins both credential directions with and without the guard, denies withheld dashboard/chart calls at the runtime allow-list gate, checks the membership reload never exposes the capture key, and confirms that an explicit guard refusal still wins over an author grant. The existing admission-resolver census now forbids helper callers that discard the principal.

## Required behavior evidence

| Case | Result | Evidence |
| --- | --- | --- |
| Owner session alone | ReadOnly=false, authoring preserved | focused-tests.log |
| Owner cookie + restricted analytics:read,sources:read Bearer | ReadOnly=true with/without middleware; dashboard/chart runtime gate denied; direct MCP dashboard creation denied; persisted dashboard delta=0 | focused-tests.log |
| Owner cookie + dashboards:write Bearer | ReadOnly=false; configured authoring tools available; MCP dashboard and chart creation both persist | focused-tests.log |
| Investigate-only runtime | Dashboard/chart tools withheld and policy denies their calls | focused-tests.log |
| Author grant plus explicit read-only guard | ReadOnly=true | focused-tests.log |
| Reviewer's mixed-credential probe after repair | Both control and restricted case pass | after-reviewer-probe.log |

These are executed auth/runtime-policy and MCP persistence checks. No external LLM conversation is claimed.

## Verification

- go build ./...: PASS.
- go vet ./...: PASS.
- Focused app/runtime/store/workloads suite: PASS, zero skips, including R01–R11 sandbox repeatability and Lohi honesty boundaries.
- Original reviewer overlay: expected FAIL before; adapted overlay: PASS after.
- Resolver census: PASS (resolver-census.log).
- Live PostgreSQL suite: all 11 AGENTRAY_LIVE_PG-gated tests plus the adjacent seeded-chart live regression, 12 tests total; see live-postgres-tests.log.
- Final full suite: PASS, exit 0; 44 packages passed, 2,336 top-level tests passed. Command: go test ./... -p 1 -count=1 -timeout=30m -v with both supplied test-database variables enabled (final-full-suite-summary.log; complete output preserved losslessly in final-full-suite.log.gz). The 25 remaining optional environmental skips are fully disclosed below.
- git diff --check: PASS.

The first verbose ./... run passed with 36 environmental skips (full-suite.log.gz). One subsequent package-parallel rerun exposed an unrelated intermittent attempt-trace ordering assertion in TestNativeLadderRunHTTPRetryEscalationAndRetention; its isolated rerun passed, and the serialized full-suite rerun passed. The final full run uses -p 1 with AGENTRAY_LIVE_PG enabled.

The coordinator explicitly accepted full-suite PASS with environmental skip disclosure and zero skips for task-relevant focused tests (msg_39367a5f7345). environmental-skips.md lists all 36 original skip reasons; 11 PostgreSQL skips are resolved, and the remaining 25 need unavailable real-provider credentials, a restricted source DB, approved Linux capacity resources, or sandbox images. No dependencies/images were installed and no skips were suppressed.

## Deviations

- **Preserve principal beside membership** — Previous repair used membership to infer all runtime authority · Found: mixed credentials borrow owner grants · Chose: retain the selected principal and separately preserve the history membership, with the guard as an additional restriction.
- **Full-suite external prerequisites** — Dispatch requested zero skips throughout · Found: 36 pre-existing opt-in skips in the verbose full run · Chose: coordinator-approved explicit disclosure and enabling the available PostgreSQL tests; focused task coverage has zero skips.
- **Admission census** — Existing test exempted authProject's admission-only resolver · Found: authProject must retain the principal for credential precedence · Chose: principalAndProject and a stricter zero-helper admission census.

## Re-run

Run bash evidence/repair-product-1b/verify.sh. The original failing probe is preserved for the starting commit; the after overlay observes the repaired options helper. The runtime remains intentionally scoped by configured agent tools as well as caller write authority.
