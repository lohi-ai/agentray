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
