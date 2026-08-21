package storage

import (
	"context"
	"fmt"
	"strings"
)

// Platform values. A product with a web app and a native app sends both through
// the same project key, so every count that does not carry this dimension blends
// two audiences into one number. ” means "we could not tell" and is rendered as
// unknown — never guessed into one of the real values.
const (
	PlatformWeb     = "web"
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
	PlatformServer  = "server"
)

// This lives in the store, not in the ingest package that calls it, because the
// column has two writers: ingest classifies each event as it arrives, and
// backfillPlatform classifies the rows written before the column existed. Two
// implementations of "which app is this" would drift the day someone adds a
// user-agent marker to one and not the other, and the drift would be invisible —
// the split would just be wrong for old rows. One table, two readers.

// nativeAppleUAs mark an Apple client that is not a browser: URLSession sets
// CFNetwork/Darwin on every request an app makes. Checked before the browser
// marker, because Safari-on-iPhone is *web* traffic and must not be counted as
// app usage.
var nativeAppleUAs = []string{"cfnetwork", "darwin"}

// nativeAndroidUAs mark a non-browser Android client (OkHttp is the default
// stack; Dalvik appears on raw HttpURLConnection).
var nativeAndroidUAs = []string{"okhttp", "dalvik"}

// bareRuntimeUAs are runtimes whose HTTP client sends its own name and nothing
// else. Node's global fetch reports exactly "node", Deno "deno", Bun "bun" —
// there is no product token and no version to substring-match on, and matching
// "node" as a substring would also claim any product with those four letters in
// its name. So they are compared whole, before the substring pass.
//
// The SDKs set `platform` explicitly and never reach this, but a hand-rolled
// `fetch` in a webhook handler is the common first integration, and landing that
// in "unknown" makes every platform split in the product wrong in the one place
// revenue arrives.
var bareRuntimeUAs = []string{"node", "undici", "deno", "bun"}

// serverUAs mark a backend HTTP client — a server-side capture, not a person on
// a device.
var serverUAs = []string{
	"curl/", "wget/", "go-http-client", "python-requests", "python-urllib",
	"node-fetch", "axios/", "got (", "postmanruntime", "insomnia", "httpie",
	"java/", "okhttp-server", "ruby", "guzzlehttp", "libwww-perl",
}

// ClassifyPlatform decides which app an event came from.
//
// An explicit property always wins: a client that says what it is knows better
// than any heuristic, and that is the contract the SDKs are written against
// (`platform` on the Swift package, `$platform` for PostHog-shaped senders).
// osName is consulted only for the mobile OS names, because on a browser it
// carries the desktop OS ("Mac OS X") and says nothing about the platform.
//
// Only then does the user agent decide, so events already in the table — sent
// before any client set the property — still classify.
func ClassifyPlatform(explicit, osName, ua string) string {
	if normalized := normalizePlatform(explicit); normalized != "" {
		return normalized
	}
	switch normalizeOS(osName) {
	case PlatformIOS:
		return PlatformIOS
	case PlatformAndroid:
		return PlatformAndroid
	}
	return platformFromUserAgent(ua)
}

// platformFromUserAgent is the heuristic half of ClassifyPlatform, split out so
// the backfill — which has only the stored user agent, never the properties —
// runs the identical ladder.
func platformFromUserAgent(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return ""
	}
	for _, sub := range nativeAppleUAs {
		if strings.Contains(lower, sub) {
			return PlatformIOS
		}
	}
	for _, sub := range nativeAndroidUAs {
		if strings.Contains(lower, sub) {
			return PlatformAndroid
		}
	}
	// Every browser — desktop, mobile Safari, mobile Chrome — sends Mozilla/5.0.
	// Reached only after the native markers, so an in-app WebView that also sets
	// CFNetwork is already resolved.
	if strings.Contains(lower, "mozilla") {
		return PlatformWeb
	}
	if strings.Contains(lower, "android") {
		return PlatformAndroid
	}
	for _, whole := range bareRuntimeUAs {
		if lower == whole {
			return PlatformServer
		}
	}
	for _, sub := range serverUAs {
		if strings.Contains(lower, sub) {
			return PlatformServer
		}
	}
	return ""
}

// normalizePlatform folds the spellings a client might send into the four stored
// values. An unrecognised value is kept as-is (lower-cased) rather than dropped:
// a product with a platform this list does not name — a CLI, a TV app, a watch —
// should still be able to split its own traffic.
func normalizePlatform(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case "":
		return ""
	case "ios", "iphone", "ipad", "ipados", "tvos", "watchos", "apple":
		return PlatformIOS
	case "android":
		return PlatformAndroid
	case "web", "browser", "js", "javascript", "site", "website":
		return PlatformWeb
	case "server", "backend", "api", "node", "python", "go", "ruby":
		return PlatformServer
	default:
		return v
	}
}

// normalizeOS maps a PostHog-style $os to a platform, for the mobile OS names
// only. "Mac OS X" / "Windows" / "Linux" return ” because they describe the
// machine a browser or a server runs on, not the app the event came from.
func normalizeOS(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case "ios", "ipados", "iphone os":
		return PlatformIOS
	case "android":
		return PlatformAndroid
	default:
		return ""
	}
}

// platformFromUserAgentSQL renders platformFromUserAgent as a ClickHouse
// expression over `col`. It is generated from the same substring tables the Go
// ladder walks, in the same order, so the two cannot disagree about which app a
// user agent belongs to — the backfill is the Go classifier, expressed as SQL.
//
// One expression rather than a mutation per user agent: ALTER … UPDATE rewrites
// the parts it touches, so N mutations is N table rewrites. This is one.
func platformFromUserAgentSQL(col string) string {
	lower := fmt.Sprintf("lower(trim(ifNull(%s, '')))", col)
	// The inputs are package constants, never user input, but the expression is
	// concatenated into DDL where a placeholder cannot go — so quote through the
	// same escaper the rest of the store's inline literals use.
	chQuoted := func(v string) string { return "'" + chIdentLiteral(v) + "'" }
	contains := func(subs []string) string {
		clauses := make([]string, 0, len(subs))
		for _, sub := range subs {
			clauses = append(clauses, fmt.Sprintf("position(%s, %s) > 0", lower, chQuoted(sub)))
		}
		return strings.Join(clauses, " OR ")
	}
	whole := func(values []string) string {
		quoted := make([]string, 0, len(values))
		for _, v := range values {
			quoted = append(quoted, chQuoted(v))
		}
		return fmt.Sprintf("%s IN (%s)", lower, strings.Join(quoted, ", "))
	}

	return strings.Join([]string{
		"multiIf(",
		fmt.Sprintf("  %s = '', '',", lower),
		fmt.Sprintf("  %s, %s,", contains(nativeAppleUAs), chQuoted(PlatformIOS)),
		fmt.Sprintf("  %s, %s,", contains(nativeAndroidUAs), chQuoted(PlatformAndroid)),
		fmt.Sprintf("  %s, %s,", contains([]string{"mozilla"}), chQuoted(PlatformWeb)),
		fmt.Sprintf("  %s, %s,", contains([]string{"android"}), chQuoted(PlatformAndroid)),
		fmt.Sprintf("  %s, %s,", whole(bareRuntimeUAs), chQuoted(PlatformServer)),
		fmt.Sprintf("  %s, %s,", contains(serverUAs), chQuoted(PlatformServer)),
		"  '')",
	}, "\n")
}

// backfillPlatform classifies the events written before the platform column
// existed. Without it the column is only as old as the deploy, so a project with
// history opens Traffic and reads "unknown" for everything it has ever collected
// — the split looks broken precisely for the customers who have enough data to
// care about it.
//
// Only rows that still carry a user agent can be recovered; the rest stay ” and
// render as unknown, which is the honest answer. Marker-guarded like
// backfillRollups: a mutation this size must run once, not on every boot.
func (s *Store) backfillPlatform(ctx context.Context) error {
	const marker = "backfill_platform_v1"
	var applied uint64
	if err := s.ch.QueryRow(ctx, `SELECT count() FROM schema_markers WHERE name = ?`, marker).Scan(&applied); err != nil {
		return fmt.Errorf("check platform backfill marker: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if err := s.ch.Exec(ctx, `
ALTER TABLE events
UPDATE platform = `+platformFromUserAgentSQL("user_agent")+`
WHERE platform = '' AND user_agent IS NOT NULL AND user_agent != ''`); err != nil {
		return fmt.Errorf("backfill platform: %w", err)
	}
	if err := s.ch.Exec(ctx, `INSERT INTO schema_markers (name) VALUES (?)`, marker); err != nil {
		return fmt.Errorf("record platform backfill marker: %w", err)
	}
	return nil
}
