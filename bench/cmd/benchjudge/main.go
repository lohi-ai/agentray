// benchjudge judges one candidate solution file for the Base −2 Calculator
// benchmark: it compiles the file and scores it against the hidden case set,
// printing the same feedback string an agent solver receives from its
// submit_solution tool.
//
// Usage: benchjudge <solution.go>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/lohi-ai/agentray/bench/judge"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchjudge <solution.go>")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	start := time.Now()
	v, err := judge.Judge(context.Background(), string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness error:", err)
		os.Exit(2)
	}
	fmt.Println(v.Format())
	summary, _ := json.Marshal(map[string]any{
		"passed": v.Passed, "total": v.Total, "all_passed": v.AllPassed(),
		"judge_seconds": time.Since(start).Seconds(),
	})
	fmt.Println(string(summary))
	if !v.AllPassed() {
		os.Exit(1)
	}
}
