package ingestion

import (
	"context"
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

	signup, created, err := h.waitlist.AddWaitlistSignup(c.Request().Context(), storage.WaitlistSignup{
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

	// Only a genuinely new address writes an event. A visitor who submits twice
	// is one person's demand, and counting the second press would inflate the
	// exact number the validation threshold is judged against.
	if created {
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

	// The unsubscribe link is returned to the page that submitted it — the only
	// moment this token is ever emitted — so the owner's own confirmation email
	// or thank-you page can carry it. It is never included in any listing.
	return c.JSON(http.StatusOK, map[string]any{
		"status":          1,
		"created":         created,
		"unsubscribe_url": unsubscribeURL(c, signup.UnsubscribeToken),
	})
}

// WaitlistUnsubscribe honours a removal request carried by the token from the
// signup response. No auth: the person unsubscribing is not a user of this
// product, has no account, and must not need one to leave.
func (h Handler) WaitlistUnsubscribe(c echo.Context) error {
	if h.waitlist == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "waitlist is not enabled on this server")
	}
	token := c.QueryParam("token")
	if err := h.waitlist.UnsubscribeWaitlist(c.Request().Context(), token); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1, "unsubscribed": true})
}

func unsubscribeURL(c echo.Context, token string) string {
	scheme := "https"
	if c.Request().TLS == nil && strings.HasPrefix(c.Request().Host, "localhost") {
		scheme = "http"
	}
	return scheme + "://" + c.Request().Host + "/waitlist/unsubscribe?token=" + token
}
