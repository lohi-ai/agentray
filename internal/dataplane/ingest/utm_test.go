package ingestion

import (
	"net/http"
	"testing"
)

// UTM campaign tags are lifted out of properties into dedicated columns so the
// acquisition board can group on a column instead of a JSON extract. The
// browser SDK sends them $-prefixed; a server SDK that already names them
// bare lands in the same columns.
func TestCaptureLiftsUTMTagsIntoColumns(t *testing.T) {
	events := &captureEvents{}
	h := Handler{projects: fakeProjects{}, events: events}

	rec := postCapture(t, h, "/capture", `{
		"api_key": "good-key",
		"event": "user.pageview",
		"distinct_id": "reader-1",
		"properties": {
			"path": "/landing",
			"$utm_source": "newsletter",
			"$utm_medium": "email",
			"$utm_campaign": "launch-week",
			"$utm_term": "novel-reader",
			"$utm_content": "hero-cta"
		}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture: %d %s", rec.Code, rec.Body.String())
	}
	ev := events.got[0]
	if ev.UTMSource != "newsletter" || ev.UTMMedium != "email" || ev.UTMCampaign != "launch-week" || ev.UTMTerm != "novel-reader" || ev.UTMContent != "hero-cta" {
		t.Fatalf("utm columns = %q/%q/%q/%q/%q, want newsletter/email/launch-week/novel-reader/hero-cta",
			ev.UTMSource, ev.UTMMedium, ev.UTMCampaign, ev.UTMTerm, ev.UTMContent)
	}

	// A non-browser sender that already uses the bare names populates the same
	// columns — the board must not depend on which SDK emitted the tag.
	rec = postCapture(t, h, "/capture", `{
		"api_key": "good-key",
		"event": "user.pageview",
		"distinct_id": "reader-2",
		"properties": {"path": "/pricing", "utm_source": "google", "utm_campaign": "brand", "utm_content": "text-link"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare-key capture: %d %s", rec.Code, rec.Body.String())
	}
	ev = events.got[1]
	if ev.UTMSource != "google" || ev.UTMCampaign != "brand" || ev.UTMContent != "text-link" || ev.UTMMedium != "" || ev.UTMTerm != "" {
		t.Fatalf("bare utm columns = %q/%q/%q/%q/%q, want google//brand//text-link",
			ev.UTMSource, ev.UTMMedium, ev.UTMCampaign, ev.UTMTerm, ev.UTMContent)
	}

	// An untagged visit stores empty strings, matching the referrer_channel
	// convention the breakdowns group on.
	rec = postCapture(t, h, "/capture", `{
		"api_key": "good-key",
		"event": "user.pageview",
		"distinct_id": "reader-3",
		"properties": {"path": "/"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("untagged capture: %d %s", rec.Code, rec.Body.String())
	}
	ev = events.got[2]
	if ev.UTMSource != "" || ev.UTMMedium != "" || ev.UTMCampaign != "" || ev.UTMTerm != "" || ev.UTMContent != "" {
		t.Fatalf("untagged utm columns = %q/%q/%q/%q/%q, want all empty",
			ev.UTMSource, ev.UTMMedium, ev.UTMCampaign, ev.UTMTerm, ev.UTMContent)
	}
}
