package agentruntime

import "testing"

func TestNativeGeminiControlsAndDurability(t *testing.T) {
	testNativeRunnerHTTPProviderControlsAndDurability(t, false, "google", false)
}
func TestNativeGeminiChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, false, false, false, false, "google")
}
