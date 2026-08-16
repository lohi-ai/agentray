package ingestion

import (
	"context"
	"html"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// waitlist.go — the one public write that is not an event.
//
// A waitlist submission is two things at once, and it has to be both or it is
// useless:
//
//  1. A CONTACT — a person who asked to be told when this ships. That belongs in
//     Postgres: deduped per address, exportable, deletable, and still there in
//     two years. The event store is a 1-year-TTL append-only log; a contact list
//     kept there is one that quietly expires.
//  2. An EVENT — `waitlist.joined`, written down the normal ingest path with the
//     same enrichment (referrer → channel, UA → visitor class) every other event
//     gets. That is what makes the signup show up in funnels, in dashboards, and
//     in every agent tool, with no new read path anywhere.
//
// `distinct_id` is what welds the two together, and it is the ONLY thing that
// crosses: the visitor who fired user.pageview on the landing page fires
// waitlist.joined under the same id, so "40 signups from 500 visitors" is a real
// conversion rate rather than two unrelated counts — while the address itself
// never leaves Postgres, where deleting it still means something.

type waitlistStore interface {
	AddWaitlistSignup(ctx context.Context, sn storage.WaitlistSignup) (storage.WaitlistSignup, bool, error)
	UnsubscribeWaitlist(ctx context.Context, token string) error
}

// WithWaitlist enables the waitlist endpoints. A zero Handler (no store) answers
// 501 rather than panicking, matching the catalog guard's degrade-cleanly shape.
func (h Handler) WithWaitlist(store waitlistStore) Handler {
	h.waitlist = store
	return h
}

type waitlistPayload struct {
	APIKey string `json:"api_key"`
	Token  string `json:"token"`
	Email  string `json:"email"`
	// Consent must be true. This is the only place AgentRay stores an address
	// belonging to someone who is not its user, so the affirmative act is
	// required at the door rather than assumed from the submission.
	Consent     bool           `json:"consent"`
	ConsentText string         `json:"consent_text"`
	Source      string         `json:"source"`
	DistinctID  string         `json:"distinct_id"`
	Properties  map[string]any `json:"properties"`
}

// Waitlist records one signup and emits the matching event.
func (h Handler) Waitlist(c echo.Context) error {
	if h.waitlist == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "waitlist is not enabled on this server")
	}
	var payload waitlistPayload
	if err := c.Bind(&payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	apiKey := firstNonEmpty(payload.APIKey, payload.Token, c.Request().Header.Get("X-API-Key"))
	project, err := h.projects.ProjectByAPIKey(c.Request().Context(), apiKey)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
	}
	if !payload.Consent {
		return echo.NewHTTPError(http.StatusBadRequest, "consent is required")
	}
	email := strings.TrimSpace(payload.Email)
	referrer := stringProp(payload.Properties, "$referrer")

	// The visitor's own id when the page sent one, so the signup lands on the
	// same person as their pageviews. Falling back to the email keeps the row
	// self-consistent for a bare HTML form with no SDK on the page at all.
	distinctID := firstNonEmpty(payload.DistinctID, strings.ToLower(email))

	_, joined, err := h.waitlist.AddWaitlistSignup(c.Request().Context(), storage.WaitlistSignup{
		ProjectID:   project.ID,
		Email:       email,
		Source:      payload.Source,
		Referrer:    referrer,
		DistinctID:  distinctID,
		ConsentText: payload.ConsentText,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	// Only a genuine join writes an event. A visitor who submits twice is one
	// person's demand, and counting the second press would inflate the exact
	// number the validation threshold is judged against.
	if joined {
		props := map[string]any{}
		for k, v := range payload.Properties {
			props[k] = v
		}
		props["source"] = payload.Source
		// The address is deliberately NOT copied into the event — not as a
		// property, and not as a `$set` trait either, which would fold it into
		// the person profile and put it in the event store for good.
		//
		// The event store is append-only with a 1-year TTL; the contact table is
		// not. Writing the address to both would mean DeleteWaitlistSignup
		// removes the row the owner can see and leaves the copy they cannot,
		// which turns "remove my data" into a lie the product tells on the
		// owner's behalf. The link that matters is distinct_id, and that is
		// already on both sides: the same visitor who fired user.pageview fires
		// waitlist.joined, so the conversion rate is real without the address
		// ever leaving Postgres.
		join, err := h.toEvent(c, capturePayload{
			APIKey: apiKey, Event: "waitlist.joined", DistinctID: distinctID, Properties: props,
		}, apiKey)
		if err != nil {
			return err
		}
		if err := h.events.InsertEvents(c.Request().Context(), []storage.Event{join}); err != nil {
			return err
		}
	}

	// One reply, byte for byte, whoever submitted and whatever was already on
	// the list.
	//
	// This endpoint is called with the project's PUBLIC write key, which ships in
	// the customer's landing-page JavaScript — so anyone at all can post to it.
	// An earlier version answered with `created` and an `unsubscribe_url`, which
	// made it two things it must never be: an oracle answering "is this address
	// on your list?" for any address someone cares to try, and a dispenser of
	// that subscriber's own unsubscribe token, which would let a stranger remove
	// them. The token is a capability held by the row; it does not leave the
	// server through a door the whole internet can knock on, and no listing
	// carries it either.
	return c.JSON(http.StatusOK, map[string]any{"status": 1})
}

// WaitlistUnsubscribePage asks before it acts.
//
// The link travels in mail, and mail is full of machines that follow links
// without a human: Gmail and Outlook prefetch, corporate URL scanners detonate
// every URL in a message, browsers preload. If the GET performed the removal,
// every one of those would unsubscribe the recipient before they ever saw the
// page — and because the subscriber count is the number the validation test is
// judged against, the owner's evidence would quietly drain away with it. So the
// GET is a page with a button and the removal is a POST, which no prefetcher
// issues.
func (h Handler) WaitlistUnsubscribePage(c echo.Context) error {
	if h.waitlist == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "waitlist is not enabled on this server")
	}
	token := c.QueryParam("token")
	if strings.TrimSpace(token) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "token is required")
	}
	return c.HTML(http.StatusOK, `<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Leave this waitlist</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:12vh auto;padding:0 1.5rem">
<h1 style="font-size:1.25rem">Leave this waitlist?</h1>
<p>You will stop hearing about this product. Nothing else happens.</p>
<form method="post" action="/waitlist/unsubscribe">
<input type="hidden" name="token" value="`+html.EscapeString(token)+`">
<button type="submit" style="font:inherit;padding:.6rem 1rem;border-radius:.5rem;border:1px solid #888;cursor:pointer">Unsubscribe me</button>
</form>
</body>`)
}

// WaitlistUnsubscribe honours a removal request carried by the token from the
// signup. No auth: the person unsubscribing is not a user of this product, has
// no account, and must not need one to leave.
func (h Handler) WaitlistUnsubscribe(c echo.Context) error {
	if h.waitlist == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "waitlist is not enabled on this server")
	}
	token := firstNonEmpty(c.FormValue("token"), c.QueryParam("token"))
	if err := h.waitlist.UnsubscribeWaitlist(c.Request().Context(), token); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if strings.Contains(c.Request().Header.Get("Accept"), "text/html") {
		return c.HTML(http.StatusOK, `<!doctype html><meta charset="utf-8">
<title>You have been removed</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:12vh auto;padding:0 1.5rem">
<p>You have been removed from this waitlist.</p></body>`)
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1, "unsubscribed": true})
}

// UnsubscribeURL builds the link for a token. It is handed to the OWNER through
// their authenticated contact export — never to the page that submitted the
// form, which is called with the public write key and so is the whole internet.
func UnsubscribeURL(c echo.Context, token string) string {
	scheme := "https"
	if c.Request().TLS == nil && strings.HasPrefix(c.Request().Host, "localhost") {
		scheme = "http"
	}
	return scheme + "://" + c.Request().Host + "/waitlist/unsubscribe?token=" + token
}
