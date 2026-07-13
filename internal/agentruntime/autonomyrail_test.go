package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/internal/storage"
	"github.com/lohi-ai/agentray/sandbox"
)

// railFixture is a representative resolved tool set: the external-write
// http_request next to internal tools that must never be touched by the rail.
func railFixture() []agentcore.Tool {
	return []agentcore.Tool{
		fakeTool{name: httptool.ToolHTTPRequest},
		fakeTool{name: sandbox.ToolReadFile},
		fakeTool{name: "run_sql"},
	}
}

// TestAutonomyRailStripsExternalWriteFromBackgroundRuns pins the acceptance
// matrix: http_request is absent in scheduled/webhook/delegate runs at
// suggest/scheduled autonomy — the hard unattended-publish rail.
func TestAutonomyRailStripsExternalWriteFromBackgroundRuns(t *testing.T) {
	for _, trigger := range []string{"scheduled", "webhook", "delegate"} {
		for _, autonomy := range []string{storage.AutonomySuggest, storage.AutonomyScheduled} {
			out := applyAutonomyRail(railFixture(), trigger, autonomy)
			names := toolNames(out)
			if names[httptool.ToolHTTPRequest] {
				t.Errorf("trigger=%s autonomy=%s: http_request must be stripped", trigger, autonomy)
			}
			if !names[sandbox.ToolReadFile] || !names["run_sql"] || len(out) != 2 {
				t.Errorf("trigger=%s autonomy=%s: internal tools must survive, got %v", trigger, autonomy, names)
			}
		}
	}
}

// TestAutonomyRailAutoKeepsExternalWrite pins the opt-in rung: at autonomy
// 'auto' a background run keeps http_request.
func TestAutonomyRailAutoKeepsExternalWrite(t *testing.T) {
	for _, trigger := range []string{"scheduled", "webhook", "delegate"} {
		out := applyAutonomyRail(railFixture(), trigger, storage.AutonomyAuto)
		if names := toolNames(out); !names[httptool.ToolHTTPRequest] || len(out) != 3 {
			t.Errorf("trigger=%s autonomy=auto: http_request must be kept, got %v", trigger, names)
		}
	}
}

// TestAutonomyRailInteractiveRunsUntouched pins that chat/manual runs keep
// their full tool set at every autonomy — the rail only governs unattended
// runs. The empty autonomy (an agent_configs row predating the ladder resolves
// to the 'suggest' default upstream, but the rail must not depend on that)
// behaves like the strictest rung for background runs and is a no-op here.
func TestAutonomyRailInteractiveRunsUntouched(t *testing.T) {
	for _, trigger := range []string{"chat", "manual"} {
		for _, autonomy := range []string{storage.AutonomySuggest, storage.AutonomyScheduled, storage.AutonomyAuto, ""} {
			out := applyAutonomyRail(railFixture(), trigger, autonomy)
			if names := toolNames(out); !names[httptool.ToolHTTPRequest] || len(out) != 3 {
				t.Errorf("trigger=%s autonomy=%q: interactive run must keep all tools, got %v", trigger, autonomy, names)
			}
		}
	}
}

// TestAutonomyRailUnknownAutonomyFailsClosed pins that any value that is not
// exactly 'auto' — including empty (existing agents that never saved a config)
// and garbage — strips external-write tools from background runs.
func TestAutonomyRailUnknownAutonomyFailsClosed(t *testing.T) {
	for _, autonomy := range []string{"", "AUTO", "full", "yolo"} {
		out := applyAutonomyRail(railFixture(), "scheduled", autonomy)
		if names := toolNames(out); names[httptool.ToolHTTPRequest] {
			t.Errorf("autonomy=%q: rail must fail closed, http_request present", autonomy)
		}
	}
}

// TestToolExternalWriteMarks pins the catalog markers: http_request is the
// external-write tool; read-only fetchers, workspace tools, and unregistered
// (run-derived) names are not.
func TestToolExternalWriteMarks(t *testing.T) {
	if !ToolExternalWrite(httptool.ToolHTTPRequest) {
		t.Error("http_request must be marked external-write")
	}
	for _, name := range []string{httptool.ToolWebFetch, sandbox.ToolReadFile, sandbox.ToolWriteFile, sandbox.ToolRunShell, "run_sql", "team_board", "not_a_tool"} {
		if ToolExternalWrite(name) {
			t.Errorf("%s must not be marked external-write", name)
		}
	}
}
