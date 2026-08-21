package ingestion

import "strings"

const (
	VisitorHuman      = "human"
	VisitorSearchBot  = "search-bot"
	VisitorAIPlatform = "ai-platform"
)

const (
	ChannelDirect   = "direct"
	ChannelInternal = "internal"
	ChannelSearch   = "search"
	ChannelSocial   = "social"
	ChannelAI       = "ai-referral"
	ChannelReferral = "referral"
)

var searchBotUAs = []string{
	"googlebot", "bingbot", "slurp", "duckduckbot", "baiduspider",
	"yandexbot", "sogou", "exabot", "applebot", "semrushbot",
}

var aiPlatformUAs = []string{
	"gptbot", "claudebot", "perplexitybot", "oai-searchbot",
	"google-extended", "bytespider",
}

var aiReferralHosts = []string{
	"chatgpt.com", "perplexity.ai", "gemini.google.com",
	"copilot.microsoft.com", "claude.ai",
}

var socialHosts = []string{
	"facebook.com", "twitter.com", "x.com", "instagram.com",
	"linkedin.com", "tiktok.com", "reddit.com", "youtube.com", "t.co",
}

var searchHostSubstrings = []string{
	"google.", "bing.com", "duckduckgo.com", "yahoo.com", "baidu.com", "yandex.",
}

var internalHostSuffixes = []string{
	"lohi2.com", "localhost",
}

func classifyUA(ua string) (visitorClass, botName string) {
	lower := strings.ToLower(ua)
	for _, sub := range aiPlatformUAs {
		if strings.Contains(lower, sub) {
			return VisitorAIPlatform, sub
		}
	}
	for _, sub := range searchBotUAs {
		if strings.Contains(lower, sub) {
			return VisitorSearchBot, sub
		}
	}
	return VisitorHuman, ""
}

func classifyReferrer(referrer string) (host, channel string) {
	host = parseReferrerHost(referrer)
	if host == "" {
		return "", ChannelDirect
	}
	for _, h := range internalHostSuffixes {
		if host == h || strings.HasSuffix(host, "."+h) {
			return host, ChannelInternal
		}
	}
	for _, h := range aiReferralHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return host, ChannelAI
		}
	}
	for _, h := range socialHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return host, ChannelSocial
		}
	}
	for _, sub := range searchHostSubstrings {
		if strings.Contains(host, sub) {
			return host, ChannelSearch
		}
	}
	return host, ChannelReferral
}

func parseReferrerHost(referrer string) string {
	if referrer == "" {
		return ""
	}
	s := referrer
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

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
var bareRuntimeUAs = map[string]bool{"node": true, "undici": true, "deno": true, "bun": true}

// serverUAs mark a backend HTTP client — a server-side capture, not a person on
// a device.
var serverUAs = []string{
	"curl/", "wget/", "go-http-client", "python-requests", "python-urllib",
	"node-fetch", "axios/", "got (", "postmanruntime", "insomnia", "httpie",
	"java/", "okhttp-server", "ruby", "guzzlehttp", "libwww-perl",
}

// classifyPlatform decides which app an event came from.
//
// An explicit property always wins: a client that says what it is knows better
// than any heuristic, and that is the contract the SDKs are written against
// (`platform` on the Swift package, `$platform` for PostHog-shaped senders).
// `$os` is consulted only for the mobile OS names, because on a browser it
// carries the desktop OS ("Mac OS X") and says nothing about the platform.
//
// Only then does the user agent decide, so events already in the table — sent
// before any client set the property — still classify.
func classifyPlatform(props map[string]any, ua string) string {
	if explicit := normalizePlatform(firstNonEmpty(
		stringProp(props, "platform"),
		stringProp(props, "$platform"),
	)); explicit != "" {
		return explicit
	}
	switch normalizeOS(stringProp(props, "$os")) {
	case PlatformIOS:
		return PlatformIOS
	case PlatformAndroid:
		return PlatformAndroid
	}

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
	if bareRuntimeUAs[lower] {
		return PlatformServer
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
