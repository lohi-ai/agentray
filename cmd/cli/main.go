// Command agentray is the client CLI: the user-side adapter in the
// agent -> cli -> API path. Its operation command set IS the shared operation
// registry — the same definitions the server exposes as REST and runs as
// in-process agent tools — so there is nothing to keep in sync. The CLI holds
// no database or queue access; it speaks only HTTP to an AgentRay server,
// mirroring the agent's least-privilege boundary.
//
// Account lifecycle (see auth.go) makes the CLI self-serve for agents:
//
//	agentray signup --email you@example.com     # create account + project
//	agentray login  --email you@example.com     # session saved to ~/.agentray
//	agentray key                                # print the project API key
//	export AGENTRAY_API_KEY=$(agentray key)
//
// Operations:
//
//	agentray ops                       # list available operations + schemas
//	agentray run_sql '{"sql":"SELECT 1"}'
//	agentray --url https://agentray.lohi2.com --key $KEY activity_summary '{"hours":24}'
//
// After login the saved server URL and project key are the defaults, so bare
// `agentray activity_summary '{"hours":24}'` works with no flags or env.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/usecase"
)

func main() {
	cfg := loadConfig()
	base := flag.String("url", firstNonEmpty(os.Getenv("AGENTRAY_URL"), cfg.URL, "http://localhost:8088"), "AgentRay API base URL")
	key := flag.String("key", firstNonEmpty(os.Getenv("AGENTRAY_API_KEY"), cfg.APIKey), "project API key (X-API-Key)")
	flag.Parse()
	args := flag.Args()

	reg := usecase.Registry()

	if len(args) == 0 || args[0] == "help" {
		printUsage(reg)
		return
	}

	switch args[0] {
	case "signup", "login", "logout", "whoami", "key", "projects":
		if err := runAccountCommand(*base, args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	case "ops":
		printUsage(reg)
		return
	}

	op := args[0]
	if _, ok := reg.Get(op); !ok {
		fmt.Fprintf(os.Stderr, "unknown command or operation %q\n\n", op)
		printUsage(reg)
		os.Exit(2)
	}

	if *key == "" {
		fmt.Fprintln(os.Stderr, "error: no API key — run `agentray login` then `agentray key`, or pass --key / AGENTRAY_API_KEY")
		os.Exit(2)
	}

	input := "{}"
	if len(args) > 1 {
		input = args[1]
	}

	client := opcore.NewClient(*base, *key)
	out, err := client.Call(context.Background(), op, []byte(input))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(string(pretty(out)))
}

// printUsage lists account commands plus the operation registry: name, summary,
// and input schema for each operation. Generated from the single source.
func printUsage(reg *opcore.Registry) {
	fmt.Println("Usage: agentray [--url URL] [--key KEY] <command|operation> ['<json-input>']")
	fmt.Println("\nAccount:")
	fmt.Println("  signup                 create an account (+ workspace + project), save session")
	fmt.Println("  login                  log in; session + default project key saved to ~/.agentray")
	fmt.Println("  logout                 revoke the session and clear saved credentials")
	fmt.Println("  whoami                 show the logged-in user and default project")
	fmt.Println("  key                    print the project API key (--project, --rotate)")
	fmt.Println("  projects               list projects in your workspaces")
	fmt.Println("\nOperations (require an API key):")
	for _, s := range reg.Specs() {
		fmt.Printf("  %-22s %s\n", s.OpName(), s.OpSummary())
	}
}

func pretty(b []byte) []byte {
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") != nil {
		return b
	}
	return buf.Bytes()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
