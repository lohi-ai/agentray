# Full Go suite result

Command: `AGENTRAY_TEST_DATABASE_URL=postgres://long@127.0.0.1:5433/agentray_test?sslmode=disable AGENTRAY_LIVE_PG=$AGENTRAY_TEST_DATABASE_URL go test ./... -p 1 -count=1 -timeout=30m -v`

Result: PASS, exit code 0. All packages completed; 25 environment-dependent tests skipped. The complete verbose output is in `full-suite.log.gz`, and skipped test names are listed in `environmental-skip-names.txt`.
