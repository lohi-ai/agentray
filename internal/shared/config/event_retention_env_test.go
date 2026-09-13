package config

import "testing"

// TestEventRetentionEnv pins the retention window's operator contract: the
// default restores the bound the ClickHouse schema enforced before the DuckDB
// port, an explicit 0 is "keep everything", and a negative window is a typo
// that falls back to the default instead of silently removing the only bound
// on the event log.
//
// Every case blanks the variable first: envInt reads the real environment, so a
// developer (or CI) running the suite with EVENT_RETENTION_DAYS exported would
// otherwise decide the "unset" case's result.
func TestEventRetentionEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantDay int
	}{
		{name: "unset keeps the parity default", wantDay: 365},
		{name: "an empty value is the same as unset", env: map[string]string{"EVENT_RETENTION_DAYS": ""}, wantDay: 365},
		{name: "operator opts out of the bound", env: map[string]string{"EVENT_RETENTION_DAYS": "0"}, wantDay: 0},
		{name: "operator shortens the window", env: map[string]string{"EVENT_RETENTION_DAYS": "30"}, wantDay: 30},
		{name: "negative is a typo, not a policy", env: map[string]string{"EVENT_RETENTION_DAYS": "-1"}, wantDay: 365},
		{name: "garbage falls back to the default", env: map[string]string{"EVENT_RETENTION_DAYS": "forever"}, wantDay: 365},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EVENT_RETENTION_DAYS", tc.env["EVENT_RETENTION_DAYS"])
			if got := FromEnv().EventRetentionDays; got != tc.wantDay {
				t.Errorf("EventRetentionDays = %d, want %d", got, tc.wantDay)
			}
		})
	}
}
