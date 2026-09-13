package config

import "testing"

// TestEventRetentionEnv pins the retention window's operator contract: the
// default restores the bound the ClickHouse schema enforced before the DuckDB
// port, an explicit 0 is "keep everything", and a negative window is a typo
// that falls back to the default instead of silently removing the only bound
// on the event log.
func TestEventRetentionEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantDay int
	}{
		{name: "unset keeps the parity default", wantDay: 365},
		{name: "operator opts out of the bound", env: map[string]string{"EVENT_RETENTION_DAYS": "0"}, wantDay: 0},
		{name: "operator shortens the window", env: map[string]string{"EVENT_RETENTION_DAYS": "30"}, wantDay: 30},
		{name: "negative is a typo, not a policy", env: map[string]string{"EVENT_RETENTION_DAYS": "-1"}, wantDay: 365},
		{name: "garbage falls back to the default", env: map[string]string{"EVENT_RETENTION_DAYS": "forever"}, wantDay: 365},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := FromEnv().EventRetentionDays; got != tc.wantDay {
				t.Errorf("EventRetentionDays = %d, want %d", got, tc.wantDay)
			}
		})
	}
}
