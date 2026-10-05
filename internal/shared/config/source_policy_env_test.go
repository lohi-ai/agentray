package config

import "testing"

func TestSourcePolicyFileFromEnv(t *testing.T) {
	t.Setenv("AGENTRAY_SOURCE_POLICY_FILE", "/operator/source-policy.json")
	if got := FromEnv().SourcePolicyFile; got != "/operator/source-policy.json" {
		t.Fatalf("SourcePolicyFile=%q", got)
	}
}
