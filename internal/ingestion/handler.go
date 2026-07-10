package ingestion

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/storage"
)

type Handler struct {
	projects projectStore
	events   eventWriter
	aliases  aliasCreator
	catalog  *catalogGuard
}

type projectStore interface {
	ProjectByAPIKey(ctx context.Context, apiKey string) (storage.Project, error)
}

type eventWriter interface {
	InsertEvents(ctx context.Context, events []storage.Event) error
}

type aliasCreator interface {
	CreateAlias(ctx context.Context, projectID, anonymousID, canonicalID string) error
}

func NewHandler(projects projectStore, events eventWriter, aliases aliasCreator) Handler {
	return Handler{projects: projects, events: events, aliases: aliases}
}

// WithCatalogGuard enables the tracking-plan signal: incoming events whose name
// is absent from the project's established catalog are tagged is_unplanned. The
// guard reads the catalog through store (EventNames) on a TTL, so tagging adds no
// per-event database hit. A zero Handler (no guard) simply never tags.
func (h Handler) WithCatalogGuard(store catalogStore) Handler {
	h.catalog = newCatalogGuard(store, 0)
	return h
}

type capturePayload struct {
	APIKey     string            `json:"api_key"`
	Token      string            `json:"token"`
	Event      string            `json:"event"`
	DistinctID string            `json:"distinct_id"`
	SessionID  string            `json:"session_id"`
	Properties map[string]any    `json:"properties"`
	Timestamp  any               `json:"timestamp"`
	Groups     map[string]string `json:"groups"`
	Set        map[string]any    `json:"$set"`
	SetOnce    map[string]any    `json:"$set_once"`
}

type batchPayload struct {
	APIKey string           `json:"api_key"`
	Token  string           `json:"token"`
	Batch  []capturePayload `json:"batch"`
}

func (h Handler) Capture(c echo.Context) error {
	var payload capturePayload
	if err := c.Bind(&payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	event, err := h.toEvent(c, payload, "")
	if err != nil {
		return err
	}
	if err := h.events.InsertEvents(c.Request().Context(), []storage.Event{event}); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1})
}

func (h Handler) Batch(c echo.Context) error {
	var payload batchPayload
	if err := c.Bind(&payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	if len(payload.Batch) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "batch is required")
	}
	events := make([]storage.Event, 0, len(payload.Batch))
	for _, item := range payload.Batch {
		event, err := h.toEvent(c, item, firstNonEmpty(payload.APIKey, payload.Token))
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	if err := h.events.InsertEvents(c.Request().Context(), events); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1})
}

func (h Handler) Identify(c echo.Context) error {
	var payload capturePayload
	if err := c.Bind(&payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	payload.Event = "$identify"
	if payload.Properties == nil {
		payload.Properties = map[string]any{}
	}
	if len(payload.Set) > 0 {
		payload.Properties["$set"] = payload.Set
	}
	if len(payload.SetOnce) > 0 {
		payload.Properties["$set_once"] = payload.SetOnce
	}
	event, err := h.toEvent(c, payload, "")
	if err != nil {
		return err
	}
	if err := h.events.InsertEvents(c.Request().Context(), []storage.Event{event}); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1})
}

func (h Handler) toEvent(c echo.Context, payload capturePayload, inheritedAPIKey string) (storage.Event, error) {
	apiKey := firstNonEmpty(payload.APIKey, payload.Token, inheritedAPIKey, c.Request().Header.Get("X-API-Key"))
	project, err := h.projects.ProjectByAPIKey(c.Request().Context(), apiKey)
	if err != nil {
		return storage.Event{}, echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
	}
	if payload.Event == "" {
		return storage.Event{}, echo.NewHTTPError(http.StatusBadRequest, "event is required")
	}

	props := map[string]any{}
	for key, value := range payload.Properties {
		props[key] = value
	}
	if len(payload.Groups) > 0 {
		props["$groups"] = payload.Groups
	}
	distinctID := firstNonEmpty(payload.DistinctID, stringProp(props, "distinct_id"))
	if distinctID == "" {
		return storage.Event{}, echo.NewHTTPError(http.StatusBadRequest, "distinct_id is required")
	}
	sessionID := firstNonEmpty(payload.SessionID, stringProp(props, "$session_id"), stringProp(props, "$session_id_uuid"))
	ts := parseTimestamp(payload.Timestamp)
	propsJSON, err := json.Marshal(props)
	if err != nil {
		return storage.Event{}, echo.NewHTTPError(http.StatusBadRequest, "properties must be json")
	}

	ua := firstNonEmpty(
		stringProp(props, "$user_agent"),
		c.Request().Header.Get("User-Agent"),
	)
	visitorClass, botName := classifyUA(ua)
	referrer := stringProp(props, "$referrer")
	refHost, refChannel := classifyReferrer(referrer)

	return storage.Event{
		ProjectID:       project.ID,
		EventID:         uuid.NewString(),
		EventName:       payload.Event,
		EventType:       eventType(payload.Event),
		DistinctID:      distinctID,
		SessionID:       sessionID,
		Properties:      string(propsJSON),
		AgentID:         stringProp(props, "agent_id"),
		ToolName:        stringProp(props, "tool_name"),
		ToolInput:       optionalJSON(props["tool_input"]),
		ToolOutput:      optionalJSON(props["tool_output"]),
		TokensInput:     uint32Prop(props, "tokens_input"),
		TokensOutput:    uint32Prop(props, "tokens_output"),
		CostUSD:         float32Prop(props, "cost_usd"),
		LatencyMS:       uint32Prop(props, "latency_ms"),
		ModelName:       stringProp(props, "model_name"),
		IsError:         boolProp(props, "is_error"),
		ErrorMessage:    stringProp(props, "error_message"),
		Timestamp:       ts,
		VisitorClass:    visitorClass,
		BotName:         botName,
		ReferrerHost:    refHost,
		ReferrerChannel: refChannel,
		UserAgent:       ua,
		InsertID:        stringProp(props, "$insert_id"),
		IsUnplanned:     h.catalog.isUnplanned(c.Request().Context(), project.ID, payload.Event),
	}, nil
}

func eventType(name string) string {
	if len(name) >= 6 && name[:6] == "agent." {
		return "agent"
	}
	if len(name) >= 7 && name[:7] == "system." {
		return "system"
	}
	return "user"
}

func parseTimestamp(raw any) time.Time {
	switch v := raw.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.000", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, v); err == nil {
				return parsed.UTC()
			}
		}
	case float64:
		if v > 1_000_000_000_000 {
			return time.UnixMilli(int64(v)).UTC()
		}
		if v > 1_000_000_000 {
			return time.Unix(int64(v), 0).UTC()
		}
	}
	return time.Now().UTC()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type aliasPayload struct {
	APIKey      string `json:"api_key"`
	Token       string `json:"token"`
	AnonymousID string `json:"anonymous_id"`
	DistinctID  string `json:"distinct_id"`
}

func (h Handler) Alias(c echo.Context) error {
	var payload aliasPayload
	if err := c.Bind(&payload); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	apiKey := firstNonEmpty(payload.APIKey, payload.Token, c.Request().Header.Get("X-API-Key"))
	project, err := h.projects.ProjectByAPIKey(c.Request().Context(), apiKey)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
	}
	anonymousID := strings.TrimSpace(payload.AnonymousID)
	canonicalID := strings.TrimSpace(payload.DistinctID)
	if anonymousID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "anonymous_id is required")
	}
	if canonicalID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "distinct_id is required")
	}
	if err := h.aliases.CreateAlias(c.Request().Context(), project.ID, anonymousID, canonicalID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"status": 1})
}
