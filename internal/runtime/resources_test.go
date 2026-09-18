package agentruntime

import (
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

type resourceCloseProbe struct{ closed atomic.Int32 }

func (p *resourceCloseProbe) Close() { p.closed.Add(1) }

func TestRuntimeResourcesAreSharedAcrossShortLivedRunners(t *testing.T) {
	resources := NewRuntimeResources()
	t.Cleanup(resources.Close)
	first := NewRunner(nil, WithRuntimeResources(resources))
	second := NewRunner(nil, WithRuntimeResources(resources))

	if first.ProviderSessions != second.ProviderSessions || first.EvalSessions != second.EvalSessions || first.LSPSessions != second.LSPSessions {
		t.Fatal("short-lived runners did not share the host resource bundle")
	}
	firstSession, releaseFirst := first.acquireProviderSession("conversation")
	releaseFirst()
	secondSession, releaseSecond := second.acquireProviderSession("conversation")
	releaseSecond()
	if firstSession != secondSession {
		t.Fatal("provider conversation state did not survive Runner reconstruction")
	}
}

func TestRuntimeResourcesCloseOwnedStateExactlyOnce(t *testing.T) {
	resources := NewRuntimeResources()
	runner := NewRunner(nil, WithRuntimeResources(resources))
	session, release := runner.acquireProviderSession("conversation")
	probe := &resourceCloseProbe{}
	if got := session.State("probe", func() agentcore.ProviderSessionState { return probe }); got != probe {
		t.Fatalf("stored state = %T, want probe", got)
	}
	release()

	resources.Close()
	resources.Close()
	if got := probe.closed.Load(); got != 1 {
		t.Fatalf("provider state closed %d times, want once", got)
	}
}
