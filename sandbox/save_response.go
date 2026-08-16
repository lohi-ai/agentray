package sandbox

import (
	"context"
	"fmt"
	"strings"
)

// Saving a response to the workspace is what makes the network tools part of the
// same agent as the file and shell tools rather than a separate one that only
// speaks in context windows.
//
// Without it, everything an agent fetches has to come back through the model:
// a 4 MB CSV is truncated, a PDF is unreadable, and a dataset it wanted to
// analyze is a summary of a dataset. With it, http_request and web_fetch put the
// bytes in the same directory read_file, grep, and run_shell already work in, so
// "download this and count the rows" is two ordinary tool calls instead of an
// impossibility.
//
// The saved path goes through the same workspace guard as write_file — relative
// only, no escaping the root, no following a symlink out — so a model that
// asks to save to "../../.ssh/authorized_keys" gets an error, not a foothold.

// responseSink writes tool output into the agent's workspace. A zero value (no
// workspace configured) refuses the save with an explanation rather than
// silently returning the body inline, because an agent that asked for a file and
// got prose has no way to tell that its next step will fail.
type responseSink struct {
	fs workspaceFS
}

// save writes data to rel and returns the one-line receipt to hand the model.
// The receipt names the path the agent must use to read the file back, which is
// the workspace-relative one — never the host path, which the agent cannot use
// and should not learn.
func (s responseSink) save(ctx context.Context, tool, rel string, data []byte) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("%s: save_as must not be empty", tool)
	}
	if s.fs == nil {
		return "", fmt.Errorf("%s: save_as needs the agent workspace, which is not configured", tool)
	}
	if err := s.fs.WriteFile(ctx, rel, data); err != nil {
		return "", fmt.Errorf("%s: save %s: %w", tool, rel, err)
	}
	return fmt.Sprintf("saved %d bytes to %s", len(data), rel), nil
}

// saveAsParam is the shared schema fragment, so the two tools describe the same
// capability in the same words and an agent that has learned one has learned the
// other.
func saveAsParam() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "Optional workspace-relative path to save the response body to " +
			"(e.g. \"data/report.csv\"). When set, the body is written to the agent's " +
			"workspace instead of being returned inline — use this for anything large or " +
			"binary, then read or process it with the file and shell tools.",
	}
}
