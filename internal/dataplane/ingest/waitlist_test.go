package ingestion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

type fakeProjects struct{}

func (fakeProjects) ProjectByAPIKey(_ context.Context, key string) (storage.Project, error) {
	if key != "good-key" {
		return storage.Project{}, echo.NewHTTPError(http.StatusUnauthorized, "nope")
	}
	return storage.Project{ID: "p1"}, nil
}

type captureEvents struct{ got []storage.Event }

func (c *captureEvents) InsertEvents(_ context.Context, e []storage.Event) error {
	c.got = append(c.got, e...)
	return nil
}

type fakeWaitlist struct {
	adds         int
	unsubscribes int
	new          bool
}

func (f *fakeWaitlist) AddWaitlistSignup(_ context.Context, sn storage.WaitlistSignup) (storage.WaitlistSignup, bool, error) {
	f.adds++
	sn.ID, sn.Status, sn.UnsubscribeToken = "w1", "subscribed", "tok"
	return sn, f.new, nil
}

func (f *fakeWaitlist) UnsubscribeWaitlist(context.Context, string) error {
	f.unsubscribes++
	return nil
}

func postWaitlist(t *testing.T, h Handler, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/waitlist", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	return rec, h.Waitlist(c)
}

// The address is a contact, not an event property. If it were also written into
// the event store — as a property OR as a `$set` trait folded into the person
// profile — then DeleteWaitlistSignup would remove the copy the owner can see
// and leave the one they cannot, in an append-only table. "Remove my data" has
// to be true, so the address must never cross into events.
func TestWaitlistEventCarriesNoAddress(t *testing.T) {
	events := &captureEvents{}
	h := Handler{projects: fakeProjects{}, events: events}.WithWaitlist(&fakeWaitlist{new: true})

	if _, err := postWaitlist(t, h, `{"api_key":"good-key","email":"founder@example.com","consent":true,"distinct_id":"a-1","source":"reddit"}`); err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	if len(events.got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(events.got))
	}
	ev := events.got[0]
	if ev.EventName != "waitlist.joined" {
		t.Errorf("event name = %q", ev.EventName)
	}
	if ev.DistinctID != "a-1" {
		t.Errorf("the page's visitor id must carry through, got %q", ev.DistinctID)
	}
	if strings.Contains(strings.ToLower(ev.Properties), "founder@example.com") {
		t.Errorf("the address leaked into the event store: %s", ev.Properties)
	}
	// $set is the specific trap: it looks like metadata and is folded into the
	// person profile, which is exactly the copy a delete cannot reach.
	var props map[string]any
	if err := json.Unmarshal([]byte(ev.Properties), &props); err != nil {
		t.Fatalf("properties are not json: %v", err)
	}
	if _, found := props["$set"]; found {
		t.Errorf("$set must not be used to smuggle traits into the person profile: %s", ev.Properties)
	}
	if props["source"] != "reddit" {
		t.Errorf("attribution must survive, got %v", props["source"])
	}
}

// A visitor pressing submit twice is one person's demand. Writing a second event
// would inflate the exact count the committed threshold is judged against.
func TestWaitlistDoesNotCountTheSamePersonTwice(t *testing.T) {
	events := &captureEvents{}
	wl := &fakeWaitlist{new: false}
	h := Handler{projects: fakeProjects{}, events: events}.WithWaitlist(wl)

	if _, err := postWaitlist(t, h, `{"api_key":"good-key","email":"founder@example.com","consent":true,"distinct_id":"a-1"}`); err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	if wl.adds != 1 {
		t.Errorf("the row must still be touched, got %d writes", wl.adds)
	}
	if len(events.got) != 0 {
		t.Errorf("a repeat submit must emit no event, got %d", len(events.got))
	}
}

// Consent is not a formality here: this is the one place AgentRay stores an
// address belonging to someone who is not its user.
func TestWaitlistRefusesWithoutConsent(t *testing.T) {
	wl := &fakeWaitlist{new: true}
	h := Handler{projects: fakeProjects{}, events: &captureEvents{}}.WithWaitlist(wl)

	_, err := postWaitlist(t, h, `{"api_key":"good-key","email":"founder@example.com","consent":false}`)
	if err == nil {
		t.Fatal("a submission without consent must be refused")
	}
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
	if wl.adds != 0 {
		t.Error("nothing may be stored when consent is absent")
	}
}

func TestWaitlistRefusesAnUnknownProject(t *testing.T) {
	wl := &fakeWaitlist{new: true}
	h := Handler{projects: fakeProjects{}, events: &captureEvents{}}.WithWaitlist(wl)

	_, err := postWaitlist(t, h, `{"api_key":"bad-key","email":"founder@example.com","consent":true}`)
	if err == nil {
		t.Fatal("an unknown api key must be refused")
	}
	if wl.adds != 0 {
		t.Error("nothing may be stored for an unauthenticated project")
	}
}

// A server built without the waitlist store must say so rather than panic — the
// same degrade-cleanly shape the catalog guard has.
func TestWaitlistWithoutAStoreDegrades(t *testing.T) {
	h := Handler{projects: fakeProjects{}, events: &captureEvents{}}
	_, err := postWaitlist(t, h, `{"api_key":"good-key","email":"a@b.co","consent":true}`)
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %v", err)
	}
}

// The reply is the same bytes whoever posts and whatever is already on the list.
//
// This endpoint is called with the project's PUBLIC write key, which ships in
// the customer's landing-page JavaScript. If the body varied with what the
// server found, the whole internet would have two things it must never have: an
// oracle answering "is this address on your list?" for any address someone
// types, and — worse — that subscriber's own unsubscribe token, which would let
// a stranger remove them and quietly drain the number the validation test is
// judged against.
func TestWaitlistTellsAStrangerNothingAboutWhoIsOnTheList(t *testing.T) {
	body := `{"api_key":"good-key","email":"someone@example.com","consent":true}`

	joined, err := postWaitlist(t, Handler{projects: fakeProjects{}, events: &captureEvents{}}.
		WithWaitlist(&fakeWaitlist{new: true}), body)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	already, err := postWaitlist(t, Handler{projects: fakeProjects{}, events: &captureEvents{}}.
		WithWaitlist(&fakeWaitlist{new: false}), body)
	if err != nil {
		t.Fatalf("repeat submit: %v", err)
	}

	if joined.Body.String() != already.Body.String() {
		t.Errorf("the reply reveals whether the address was already known:\n new:  %s\n known: %s",
			joined.Body.String(), already.Body.String())
	}
	for _, rec := range []*httptest.ResponseRecorder{joined, already} {
		if strings.Contains(rec.Body.String(), "tok") {
			t.Errorf("the unsubscribe token left the server through the public form: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "unsubscribe") {
			t.Errorf("the reply carries an unsubscribe link: %s", rec.Body.String())
		}
	}
}

// The link in a signup email is followed by machines — Gmail and Outlook
// prefetch, corporate scanners detonate every URL, browsers preload. If the GET
// performed the removal, all of them would unsubscribe the recipient before a
// human ever saw the page, and the owner's evidence would drain away with it.
func TestUnsubscribeGetAsksBeforeItActs(t *testing.T) {
	wl := &fakeWaitlist{}
	h := Handler{projects: fakeProjects{}, events: &captureEvents{}}.WithWaitlist(wl)

	req := httptest.NewRequest(http.MethodGet, "/waitlist/unsubscribe?token=tok", nil)
	rec := httptest.NewRecorder()
	if err := h.WaitlistUnsubscribePage(echo.New().NewContext(req, rec)); err != nil {
		t.Fatalf("confirm page: %v", err)
	}
	if wl.unsubscribes != 0 {
		t.Fatal("a GET removed the subscriber; a prefetcher would have done the same")
	}
	if !strings.Contains(rec.Body.String(), `method="post"`) {
		t.Error("the confirm page offers no POST for the person to actually leave")
	}

	post := httptest.NewRequest(http.MethodPost, "/waitlist/unsubscribe",
		strings.NewReader("token=tok"))
	post.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	if err := h.WaitlistUnsubscribe(echo.New().NewContext(post, httptest.NewRecorder())); err != nil {
		t.Fatalf("post unsubscribe: %v", err)
	}
	if wl.unsubscribes != 1 {
		t.Fatalf("POST did not unsubscribe (calls=%d)", wl.unsubscribes)
	}
}
