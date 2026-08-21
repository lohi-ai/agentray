package ingestion

import "testing"

func TestClassifyPlatform(t *testing.T) {
	const (
		safariIOS = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
		nativeIOS = "XplatApp/1.2.0 (iPhone; iOS 18.2; Scale/3.00) CFNetwork/1568 Darwin/24.2.0"
		chromeMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36"
		okhttp    = "okhttp/4.12.0"
		dalvik    = "Dalvik/2.1.0 (Linux; U; Android 13; Pixel 7 Build/TQ3A)"
		curlAgent = "curl/8.4.0"
		goHTTP    = "Go-http-client/2.0"
		googlebot = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
		unknownUA = "SomeThing/1.0"
	)

	tests := []struct {
		name  string
		props map[string]any
		ua    string
		want  string
	}{
		{"explicit property beats the user agent", map[string]any{"platform": "ios"}, chromeMac, PlatformIOS},
		{"posthog-style $platform", map[string]any{"$platform": "Android"}, chromeMac, PlatformAndroid},
		{"explicit spelling folded", map[string]any{"platform": "iPhone"}, "", PlatformIOS},
		{"unrecognised explicit platform is kept", map[string]any{"platform": "tv"}, chromeMac, "tv"},
		{"blank explicit falls through to the agent", map[string]any{"platform": "  "}, chromeMac, PlatformWeb},
		{"$os names a mobile OS", map[string]any{"$os": "iOS"}, "", PlatformIOS},
		{"$os naming a desktop OS decides nothing", map[string]any{"$os": "Mac OS X"}, chromeMac, PlatformWeb},

		// The distinction the whole column exists for: Safari on an iPhone is a
		// person on the website, not a person in the app.
		{"safari on iphone is web", nil, safariIOS, PlatformWeb},
		{"native URLSession is ios", nil, nativeIOS, PlatformIOS},
		{"desktop browser is web", nil, chromeMac, PlatformWeb},
		{"okhttp is android", nil, okhttp, PlatformAndroid},
		{"dalvik is android", nil, dalvik, PlatformAndroid},
		{"curl is server", nil, curlAgent, PlatformServer},
		{"go client is server", nil, goHTTP, PlatformServer},

		// Node's global fetch sends the bare word "node" and nothing else, which
		// matched nothing until bareRuntimeUAs — so every revenue event from a
		// webhook handler landed in "unknown" and skewed the platform split at
		// exactly the point money arrives.
		{"bare node runtime is server", nil, "node", PlatformServer},
		{"undici is server", nil, "undici", PlatformServer},
		{"deno is server", nil, "Deno", PlatformServer},
		{"bun is server", nil, "bun", PlatformServer},
		// Whole-string only: a product whose name merely contains a runtime name
		// must not be claimed as a backend.
		{"a product named after a runtime is not a server", nil, "nodebot/1.0", ""},
		{"a crawler still reads as web", nil, googlebot, PlatformWeb},
		{"no user agent is undetermined", nil, "", ""},
		{"unrecognised user agent is undetermined", nil, unknownUA, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPlatform(tc.props, tc.ua); got != tc.want {
				t.Fatalf("classifyPlatform(%v, %q) = %q, want %q", tc.props, tc.ua, got, tc.want)
			}
		})
	}
}
