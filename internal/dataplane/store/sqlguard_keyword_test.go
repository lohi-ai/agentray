package storage

import "testing"

// The keyword denylist used a raw substring test, which rejected the ordinary
// vocabulary of commerce: `created_at` contains CREATE, `insert_id` contains
// INSERT, `subscription_granted` contains GRANT. run_sql is the only path in the
// product to a revenue number, so those false rejections denied exactly the
// money questions AgentRay is sold to answer.
func TestValidateReadonlySQLAllowsCommerceVocabulary(t *testing.T) {
	allowed := []string{
		"SELECT toStartOfDay(created_at) AS d, count() FROM events GROUP BY d",
		"SELECT JSONExtractString(properties, 'updated_at') FROM events",
		"SELECT count() FROM events WHERE event_name = 'subscription_created'",
		"SELECT count() FROM events WHERE event_name = 'subscription_granted'",
		"SELECT count() FROM events WHERE event_name = 'order_created'",
		"SELECT count() FROM events WHERE event_name = 'account_deleted'",
		"SELECT count() FROM events WHERE event_name = 'cart_updated'",
		"SELECT count() FROM events WHERE event_name = 'system.pipeline.stats'",
		"SELECT JSONExtractString(properties, '$insert_id') AS k FROM events",
		// The de-dup recipe the Data Analyst preset teaches for money totals.
		"SELECT sum(amt) FROM (SELECT insert_id, argMax(JSONExtractFloat(properties,'amount'), timestamp) AS amt FROM events WHERE event_name = 'revenue' GROUP BY insert_id)",
	}
	for _, q := range allowed {
		if err := validateReadonlySQL(q); err != nil {
			t.Errorf("commerce query wrongly rejected (%v): %s", err, q)
		}
	}
}

// Loosening the match to whole tokens must not loosen the security property: a
// real statement keyword still appears as a bare token and is still refused.
func TestValidateReadonlySQLStillRejectsStatementKeywords(t *testing.T) {
	rejected := []string{
		"SELECT * FROM system.tables",
		"SELECT 1 UNION ALL SELECT 1 FROM system.numbers",
		"WITH x AS (SELECT 1) INSERT INTO events VALUES (1)",
		"SELECT * FROM events WHERE 1=1 -- \n DROP TABLE events",
		"SELECT create(1)",
		"SELECT * FROM events GRANT SELECT",
		"SELECT * FROM events; DROP TABLE events",
		"ALTER TABLE events DELETE WHERE 1=1",
		"SELECT * FROM events TRUNCATE",
	}
	for _, q := range rejected {
		if err := validateReadonlySQL(q); err == nil {
			t.Errorf("statement keyword wrongly allowed: %s", q)
		}
	}
}

// A keyword hidden inside a literal is data, not a statement — but it must not
// become a way to smuggle one either, which the single-statement and
// SELECT/WITH-prefix rules already prevent.
func TestMaskSQLLiteralsBlanksQuotedSpans(t *testing.T) {
	cases := map[string]string{
		"WHERE a = 'DROP'":      "WHERE a = '    '",
		"WHERE a = 'it''s'":     "WHERE a = '     '",
		"WHERE a = 'a\\'b'":     "WHERE a = '    '",
		"SELECT `drop table` x": "SELECT `          ` x",
	}
	for in, want := range cases {
		if got := maskSQLLiterals(in); got != want {
			t.Errorf("maskSQLLiterals(%q) = %q, want %q", in, got, want)
		}
	}
}
