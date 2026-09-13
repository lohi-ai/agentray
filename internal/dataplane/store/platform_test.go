package storage

import (
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
