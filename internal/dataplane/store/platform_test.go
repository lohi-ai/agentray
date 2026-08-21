package storage

import (
	"strings"
	"testing"
)

const (
	uaSafariIOS = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	uaNativeIOS = "XplatApp/1.2.0 (iPhone; iOS 18.2; Scale/3.00) CFNetwork/1568 Darwin/24.2.0"
	uaChromeMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36"
	uaOkhttp    = "okhttp/4.12.0"
	uaDalvik    = "Dalvik/2.1.0 (Linux; U; Android 13; Pixel 7 Build/TQ3A)"
	uaCurl      = "curl/8.4.0"
	uaGoHTTP    = "Go-http-client/2.0"
	uaGooglebot = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	uaUnknown   = "SomeThing/1.0"
)

func TestClassifyPlatform(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		os       string
		ua       string
		want     string
	}{
		{"explicit property beats the user agent", "ios", "", uaChromeMac, PlatformIOS},
		{"posthog-style $platform", "Android", "", uaChromeMac, PlatformAndroid},
		{"explicit spelling folded", "iPhone", "", "", PlatformIOS},
		{"unrecognised explicit platform is kept", "tv", "", uaChromeMac, "tv"},
		{"blank explicit falls through to the agent", "  ", "", uaChromeMac, PlatformWeb},
		{"$os names a mobile OS", "", "iOS", "", PlatformIOS},
		{"$os naming a desktop OS decides nothing", "", "Mac OS X", uaChromeMac, PlatformWeb},

		// The distinction the whole column exists for: Safari on an iPhone is a
		// person on the website, not a person in the app.
		{"safari on iphone is web", "", "", uaSafariIOS, PlatformWeb},
		{"native URLSession is ios", "", "", uaNativeIOS, PlatformIOS},
		{"desktop browser is web", "", "", uaChromeMac, PlatformWeb},
		{"okhttp is android", "", "", uaOkhttp, PlatformAndroid},
		{"dalvik is android", "", "", uaDalvik, PlatformAndroid},
		{"curl is server", "", "", uaCurl, PlatformServer},
		{"go client is server", "", "", uaGoHTTP, PlatformServer},

		// Node's global fetch sends the bare word "node" and nothing else, which
		// matched nothing until bareRuntimeUAs — so every revenue event from a
		// webhook handler landed in "unknown" and skewed the platform split at
		// exactly the point money arrives.
		{"bare node runtime is server", "", "", "node", PlatformServer},
		{"undici is server", "", "", "undici", PlatformServer},
		{"deno is server", "", "", "Deno", PlatformServer},
		{"bun is server", "", "", "bun", PlatformServer},
		// Whole-string only: a product whose name merely contains a runtime name
		// must not be claimed as a backend.
		{"a product named after a runtime is not a server", "", "", "nodebot/1.0", ""},
		{"a crawler still reads as web", "", "", uaGooglebot, PlatformWeb},
		{"no user agent is undetermined", "", "", "", ""},
		{"unrecognised user agent is undetermined", "", "", uaUnknown, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPlatform(tc.explicit, tc.os, tc.ua); got != tc.want {
				t.Fatalf("ClassifyPlatform(%q, %q, %q) = %q, want %q", tc.explicit, tc.os, tc.ua, got, tc.want)
			}
		})
	}
}

// TestPlatformFromUserAgentSQLMirrorsGo is the guard on the backfill. The Go
// ladder decides new events and the SQL decides old ones; if they ever disagree
// the platform split is silently wrong across the boundary of a deploy, and
// nothing in the product would show it. So: every marker the Go tables carry has
// to appear in the SQL, in the same branch order, and the branches have to be the
// ones the ladder walks.
func TestPlatformFromUserAgentSQLMirrorsGo(t *testing.T) {
	sql := platformFromUserAgentSQL("user_agent")

	for _, group := range [][]string{nativeAppleUAs, nativeAndroidUAs, bareRuntimeUAs, serverUAs} {
		for _, marker := range group {
			if !strings.Contains(sql, "'"+chIdentLiteral(marker)+"'") {
				t.Errorf("marker %q is in the Go tables but not in the backfill SQL", marker)
			}
		}
	}

	// Branch order is the whole correctness argument: Safari-on-iPhone must hit
	// the browser branch, and a CFNetwork app must be claimed before it. Match the
	// predicate spellings, not the bare markers — 'android' is also a *result*
	// value two branches earlier, on the okhttp arm.
	order := []string{", 'cfnetwork')", ", 'okhttp')", ", 'mozilla')", ", 'android')", "IN ('node'", ", 'curl/')"}
	at := -1
	for _, needle := range order {
		next := strings.Index(sql, needle)
		if next < 0 {
			t.Fatalf("backfill SQL is missing %s entirely:\n%s", needle, sql)
		}
		if next <= at {
			t.Fatalf("backfill SQL puts %s out of ladder order:\n%s", needle, sql)
		}
		at = next
	}

	// A NULL user_agent must not blow up the mutation — every pre-column row that
	// never carried one has NULL there.
	if !strings.Contains(sql, "ifNull(user_agent, '')") {
		t.Errorf("backfill SQL does not guard a NULL user_agent:\n%s", sql)
	}
}
