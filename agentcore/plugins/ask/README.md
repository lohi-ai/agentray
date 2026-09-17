# ask

**Extension.** Ejectable — the loop never names it. Without it the agent can
only guess when it needs a human decision; with it the agent asks a structured
question and the run parks until the answer arrives.

## Model Experience

### A question parks the run

The model calls `ask` with a prompt, optional labeled options, and a `multi`
flag. The tool never returns a result: a valid call ends the run parked
(`ErrParked`), the call stays dangling in the durable log behind an
`EntryQuestion`, and the run row settles as `waiting`.

#### Token effect

**Zero** while parked — no model call runs until the human answers.

#### KV cache effect

**Append-only.** The answer lands as the call's tool result on resume.

### The answer is the tool result

The human's reply is recorded as an `EntryAnswer` keyed on the call id; a
resumed run closes the dangling call with that text — the model sees the
answer exactly as if `ask` had returned it. A resume that finds the question
still unanswered re-issues the call (the tool is retry-safe), which re-parks
the run on the same question.

## What it cannot do

- **Ask on unattended runs.** The host advertises it only on chat-triggered
  runs; a scheduled, webhook, delegate, or manual run never sees the tool.
- **Ask twice in one call.** One question per call; a second question is a
  second call.
- **Block the process.** Parking ends the run — nothing waits in memory, so a
  restart while parked loses nothing: the question is durable and the resume
  re-asks or delivers the stored answer.
