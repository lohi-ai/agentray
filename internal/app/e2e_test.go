//go:build e2e

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/config"
	"github.com/redis/go-redis/v9"
)

type eventsResponse struct {
	ProjectID string `json:"project_id"`
	Events    []struct {
		EventName    string  `json:"event_name"`
		DistinctID   string  `json:"distinct_id"`
		SessionID    string  `json:"session_id"`
		Properties   string  `json:"properties"`
		InsertedAt   string  `json:"inserted_at"`
		Timestamp    string  `json:"timestamp"`
		ErrorMessage string  `json:"error_message"`
		IsError      bool    `json:"is_error"`
		CostUSD      float64 `json:"cost_usd"`
	} `json:"events"`
}

type sessionsResponse struct {
	ProjectID string `json:"project_id"`
	Sessions  []struct {
		SessionID      string  `json:"session_id"`
		DistinctID     string  `json:"distinct_id"`
		EventCount     uint64  `json:"event_count"`
		TotalTokensIn  uint64  `json:"total_tokens_in"`
		TotalTokensOut uint64  `json:"total_tokens_out"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
	} `json:"sessions"`
}

type projectResponse struct {
	Project struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
		APIKey      string `json:"api_key"`
		CreatedAt   string `json:"created_at"`
	} `json:"project"`
}

type authResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Workspaces []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"workspaces"`
	Projects []struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
		APIKey      string `json:"api_key"`
	} `json:"projects"`
	Project struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
		APIKey      string `json:"api_key"`
	} `json:"project"`
}

type activityResponse struct {
	Summary struct {
		EventCount     uint64  `json:"event_count"`
		AgentEvents    uint64  `json:"agent_events"`
		DistinctUsers  uint64  `json:"distinct_users"`
		TotalTokensIn  uint64  `json:"total_tokens_in"`
		TotalTokensOut uint64  `json:"total_tokens_out"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		EventCounts    []struct {
			EventName string `json:"event_name"`
			Count     uint64 `json:"count"`
		} `json:"event_counts"`
		Timeline []struct {
			Hour  string `json:"hour"`
			Count uint64 `json:"count"`
		} `json:"timeline"`
	} `json:"summary"`
}

type dashboardResponse struct {
	Dashboard struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"dashboard"`
}

type dashboardsResponse struct {
	Dashboards []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"dashboards"`
}

type chartResponse struct {
	Chart struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Metric    string `json:"metric"`
		EventName string `json:"event_name"`
		EventType string `json:"event_type"`
	} `json:"chart"`
}

type chartsResponse struct {
	Charts []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Metric    string `json:"metric"`
		EventName string `json:"event_name"`
		EventType string `json:"event_type"`
	} `json:"charts"`
}

type insightResponse struct {
	Insight struct {
		Type   string `json:"type"`
		Series []struct {
			Hour  string `json:"hour"`
			Count uint64 `json:"count"`
		} `json:"series"`
		Funnel []struct {
			EventName  string  `json:"event_name"`
			Users      uint64  `json:"users"`
			Conversion float64 `json:"conversion"`
		} `json:"funnel"`
		Rows      []map[string]any `json:"rows"`
		Retention []struct {
			Period string  `json:"period"`
			Users  uint64  `json:"users"`
			Rate   float64 `json:"rate"`
		} `json:"retention"`
	} `json:"insight"`
}

type templatesResponse struct {
	Templates []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Charts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"charts"`
	} `json:"templates"`
}

type templateApplyResponse struct {
	Dashboard struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"dashboard"`
	Charts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"charts"`
}

type webAnalyticsResponse struct {
	WebAnalytics struct {
		Visitors    uint64 `json:"visitors"`
		Pageviews   uint64 `json:"pageviews"`
		Sessions    uint64 `json:"sessions"`
		Conversions uint64 `json:"conversions"`
		TopPaths    []struct {
			Value string `json:"value"`
			Count uint64 `json:"count"`
		} `json:"top_paths"`
		TrafficByProvider []struct {
			Class     string `json:"class"`
			Provider  string `json:"provider"`
			Visitors  uint64 `json:"visitors"`
			Pageviews uint64 `json:"pageviews"`
		} `json:"traffic_by_provider"`
		AITopPaths []struct {
			Value string `json:"value"`
			Count uint64 `json:"count"`
		} `json:"ai_top_paths"`
	} `json:"web_analytics"`
}

type explorerResponse struct {
	Explorer struct {
		Events []struct {
			EventName  string `json:"event_name"`
			DistinctID string `json:"distinct_id"`
			SessionID  string `json:"session_id"`
			Properties string `json:"properties"`
		} `json:"events"`
		Timeline []struct {
			EventName string `json:"event_name"`
		} `json:"timeline"`
	} `json:"explorer"`
}

type replayResponse struct {
	Replay struct {
		SessionID      string  `json:"session_id"`
		EventCount     uint64  `json:"event_count"`
		TotalTokensIn  uint64  `json:"total_tokens_in"`
		TotalTokensOut uint64  `json:"total_tokens_out"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Events         []struct {
			EventName string `json:"event_name"`
			ToolName  string `json:"tool_name"`
		} `json:"events"`
	} `json:"replay"`
}

type savedQueryResponse struct {
	SavedQuery struct {
		ID           string `json:"id"`
		GeneratedSQL string `json:"generated_sql"`
	} `json:"saved_query"`
}

type savedQueryRunResponse struct {
	Result struct {
		Rows []map[string]any `json:"rows"`
	} `json:"result"`
}

type personsResponse struct {
	Persons struct {
		Total      uint64 `json:"total"`
		Identified uint64 `json:"identified"`
		Anonymous  uint64 `json:"anonymous"`
		Persons    []struct {
			DistinctID string `json:"distinct_id"`
			Email      string `json:"email"`
			EventCount uint64 `json:"event_count"`
		} `json:"persons"`
	} `json:"persons"`
}

type workspaceUsageResponse struct {
	Usage struct {
		WorkspaceID   string `json:"workspace_id"`
		ProjectCount  uint64 `json:"project_count"`
		EventCount    uint64 `json:"event_count"`
		DistinctUsers uint64 `json:"distinct_users"`
	} `json:"usage"`
}

type workspaceMemberResponse struct {
	Member struct {
		WorkspaceID string `json:"workspace_id"`
		UserID      string `json:"user_id"`
		Email       string `json:"email"`
		Role        string `json:"role"`
	} `json:"member"`
}

type workspaceMembersResponse struct {
	Members []struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Role   string `json:"role"`
	} `json:"members"`
}

type workspaceAuditLogsResponse struct {
	Logs []struct {
		Action      string `json:"action"`
		ActorEmail  string `json:"actor_email"`
		TargetLabel string `json:"target_label"`
	} `json:"logs"`
}

func TestAnalyticsServiceE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for e2e test")
	}

	root := serviceRoot(t)
	project := fmt.Sprintf("agentray-e2e-%d", time.Now().UnixNano())
	infraHost := os.Getenv("AGENTRAY_E2E_INFRA_HOST")
	if infraHost == "" {
		infraHost = "127.0.0.1"
	}
	pgPort := freePort(t)
	chHTTPPort := freePort(t)
	chNativePort := freePort(t)
	redisPort := freePort(t)
	natsPort := freePort(t)

	env := append(os.Environ(),
		fmt.Sprintf("AGENTRAY_POSTGRES_PORT=%d", pgPort),
		fmt.Sprintf("AGENTRAY_CLICKHOUSE_HTTP_PORT=%d", chHTTPPort),
		fmt.Sprintf("AGENTRAY_CLICKHOUSE_NATIVE_PORT=%d", chNativePort),
		fmt.Sprintf("AGENTRAY_REDIS_PORT=%d", redisPort),
		fmt.Sprintf("AGENTRAY_NATS_PORT=%d", natsPort),
	)

	composeUp := exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(root, "docker-compose.yml"), "up", "-d", "postgres", "clickhouse", "redis", "nats")
	composeUp.Dir = root
	composeUp.Env = env
	if output, err := composeUp.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, output)
	}

	t.Cleanup(func() {
		composeDown := exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(root, "docker-compose.yml"), "down", "-v")
		composeDown.Dir = root
		composeDown.Env = env
		if output, err := composeDown.CombinedOutput(); err != nil {
			t.Logf("docker compose down failed: %v\n%s", err, output)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, pgPort))
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, redisPort))
	waitForTCP(t, ctx, fmt.Sprintf("%s:%d", infraHost, natsPort))
	waitForHTTP(t, ctx, fmt.Sprintf("http://%s:%d/ping", infraHost, chHTTPPort))

	cfg := config.Config{
		PostgresURL:          fmt.Sprintf("postgres://lohi:lohi@%s:%d/lohi_analytics?sslmode=disable", infraHost, pgPort),
		ClickHouseAddr:       fmt.Sprintf("%s:%d", infraHost, chNativePort),
		ClickHouseDatabase:   "lohi_analytics",
		ClickHouseUser:       "lohi",
		ClickHousePassword:   "lohi",
		RedisURL:             fmt.Sprintf("redis://%s:%d/0", infraHost, redisPort),
		NATSURL:              fmt.Sprintf("nats://%s:%d", infraHost, natsPort),
		IngestSubject:        "agentray.e2e.events.ingest",
		RateLimitPerMinute:   100,
		DefaultProjectName:   "AgentRay e2e",
		DefaultProjectAPIKey: "agentray_e2e_token",
		AllowedOrigins:       "http://localhost:3100,http://127.0.0.1:3100",
	}
	redisClient, err := openRedis(cfg.RedisURL)
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	t.Cleanup(func() {
		_ = redisClient.Close()
	})

	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("shutdown failed: %v", err)
		}
	})

	ts := httptest.NewServer(srv.echo)
	defer ts.Close()
	client := ts.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client.Jar = jar

	assertStatus(t, client, http.MethodGet, ts.URL+"/healthz", nil, http.StatusOK)

	var signup authResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/auth/signup", map[string]any{
		"email":          "admin-e2e@example.com",
		"name":           "AgentRay E2E Admin",
		"password":       "agentray-e2e",
		"workspace_name": "AgentRay e2e workspace",
		"project_name":   "AgentRay bootstrap e2e",
	}, &signup, http.StatusCreated)
	if signup.User.ID == "" || signup.Project.ID == "" || len(signup.Workspaces) != 1 {
		t.Fatalf("signup did not return account resources: %+v", signup)
	}

	var me authResponse
	getJSONMust(t, client, ts.URL+"/api/auth/me", &me)
	if me.User.Email != "admin-e2e@example.com" || me.Project.ID != signup.Project.ID {
		t.Fatalf("unexpected auth/me response: %+v", me)
	}

	var emptyWeb webAnalyticsResponse
	getJSONMust(t, client, ts.URL+"/api/web-analytics?project_id="+signup.Project.ID, &emptyWeb)
	if emptyWeb.WebAnalytics.Visitors != 0 || emptyWeb.WebAnalytics.Pageviews != 0 {
		t.Fatalf("empty project web analytics should be zeroed: %+v", emptyWeb.WebAnalytics)
	}

	var createdProject projectResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/projects", map[string]any{
		"workspace_id": signup.Workspaces[0].ID,
		"name":         "AgentRay managed e2e",
	}, &createdProject, http.StatusCreated)
	if createdProject.Project.ID == "" || createdProject.Project.APIKey == "" || createdProject.Project.WorkspaceID != signup.Workspaces[0].ID {
		t.Fatalf("created project is missing id/api key: %+v", createdProject.Project)
	}

	collabJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("collab cookie jar: %v", err)
	}
	collabClient := &http.Client{Jar: collabJar, Timeout: 10 * time.Second}
	var collaborator authResponse
	requestJSON(t, collabClient, http.MethodPost, ts.URL+"/api/auth/signup", map[string]any{
		"email":          "collab-e2e@example.com",
		"name":           "AgentRay E2E Collaborator",
		"password":       "agentray-e2e",
		"workspace_name": "Collaborator workspace",
		"project_name":   "Collaborator project",
	}, &collaborator, http.StatusCreated)

	var addedMember workspaceMemberResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/members", map[string]any{
		"email": "collab-e2e@example.com",
		"role":  "member",
	}, &addedMember, http.StatusCreated)
	if addedMember.Member.Email != "collab-e2e@example.com" || addedMember.Member.Role != "member" {
		t.Fatalf("unexpected added member: %+v", addedMember.Member)
	}
	var members workspaceMembersResponse
	getJSONMust(t, client, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/members", &members)
	if len(members.Members) < 2 {
		t.Fatalf("workspace members missing owner/collaborator: %+v", members.Members)
	}
	var collaboratorProjects struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	getJSONMust(t, collabClient, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/projects", &collaboratorProjects)
	requestJSON(t, collabClient, http.MethodPost, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/projects", map[string]any{"name": "blocked"}, nil, http.StatusForbidden)

	var promotedMember workspaceMemberResponse
	requestJSON(t, client, http.MethodPut, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/members/"+addedMember.Member.UserID, map[string]any{
		"role": "admin",
	}, &promotedMember, http.StatusOK)
	if promotedMember.Member.Role != "admin" {
		t.Fatalf("member role was not updated: %+v", promotedMember.Member)
	}
	assertStatus(t, client, http.MethodDelete, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/members/"+addedMember.Member.UserID, nil, http.StatusNoContent)
	if collaborator.User.ID == "" {
		t.Fatalf("collaborator signup did not return user: %+v", collaborator.User)
	}

	var rotatedProject projectResponse
	requestJSON(t, client, http.MethodPost, fmt.Sprintf("%s/api/projects/%s/rotate-key", ts.URL, createdProject.Project.ID), map[string]any{}, &rotatedProject, http.StatusOK)
	if rotatedProject.Project.APIKey == "" || rotatedProject.Project.APIKey == createdProject.Project.APIKey {
		t.Fatalf("project key was not rotated: old=%q new=%q", createdProject.Project.APIKey, rotatedProject.Project.APIKey)
	}

	var audit workspaceAuditLogsResponse
	getJSONMust(t, client, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/audit-logs?limit=10", &audit)
	for _, action := range []string{"member.upserted", "member.role_updated", "member.removed", "project.key_rotated"} {
		if !auditHasAction(audit, action) {
			t.Fatalf("audit log missing %s: %+v", action, audit.Logs)
		}
	}
	activeKey := rotatedProject.Project.APIKey
	noSessionClient := &http.Client{Timeout: 5 * time.Second}
	assertStatus(t, noSessionClient, http.MethodPost, fmt.Sprintf("%s/api/projects/%s/rotate-key?api_key=%s", ts.URL, createdProject.Project.ID, activeKey), []byte(`{}`), http.StatusUnauthorized)

	postJSON(t, client, ts.URL+"/identify", map[string]any{
		"api_key":     activeKey,
		"distinct_id": "user-e2e-1",
		"$set": map[string]any{
			"email": "e2e@example.com",
			"name":  "E2E User",
		},
	})

	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "agent.tool_call",
		"distinct_id": "user-e2e-1",
		"session_id":  "session-e2e-1",
		"properties": map[string]any{
			"agent_id":      "agent-e2e",
			"model_name":    "gpt-e2e",
			"tool_name":     "search",
			"latency_ms":    120,
			"tokens_input":  20,
			"tokens_output": 8,
			"cost_usd":      0.001,
		},
	})

	postJSON(t, client, ts.URL+"/batch", map[string]any{
		"token": activeKey,
		"batch": []map[string]any{
			{
				"event":       "agent.tool_result",
				"distinct_id": "user-e2e-1",
				"session_id":  "session-e2e-1",
				"properties": map[string]any{
					"agent_id":      "agent-e2e",
					"model_name":    "gpt-e2e",
					"tool_name":     "search",
					"latency_ms":    30,
					"tokens_input":  3,
					"tokens_output": 2,
					"cost_usd":      0.0002,
				},
			},
			{
				"event":       "user.pageview",
				"distinct_id": "user-e2e-1",
				"properties": map[string]any{
					"path":      "/pricing",
					"$referrer": "https://example.com",
				},
			},
			{
				"event":       "user.conversion",
				"distinct_id": "user-e2e-1",
				"properties": map[string]any{
					"path": "/pricing",
				},
			},
		},
	})

	oldSessionID := "session-e2e-old"
	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "agent.tool_call",
		"distinct_id": "user-e2e-old",
		"session_id":  oldSessionID,
		"timestamp":   time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano),
		"properties": map[string]any{
			"agent_id":      "agent-e2e",
			"model_name":    "gpt-e2e",
			"tool_name":     "search",
			"tokens_input":  5,
			"tokens_output": 7,
		},
	})

	waitForCondition(t, ctx, "redis rate-limit state", func() error {
		return redisClient.Ping(ctx).Err()
	}, func() bool {
		keys, err := redisClient.Keys(ctx, "agentray:rate:*").Result()
		if err != nil {
			return false
		}
		return len(keys) > 0
	})

	var events eventsResponse
	waitForCondition(t, ctx, "events readback", func() error {
		return getJSON(client, ts.URL+"/api/events?api_key="+activeKey+"&limit=10", &events)
	}, func() bool {
		names := make([]string, 0, len(events.Events))
		for _, event := range events.Events {
			names = append(names, event.EventName)
		}
		return slices.Contains(names, "$identify") &&
			slices.Contains(names, "agent.tool_call") &&
			slices.Contains(names, "agent.tool_result") &&
			slices.Contains(names, "user.pageview")
	})

	var sessions sessionsResponse
	waitForCondition(t, ctx, "session aggregates", func() error {
		return getJSON(client, ts.URL+"/api/sessions?api_key="+activeKey+"&limit=10", &sessions)
	}, func() bool {
		for _, session := range sessions.Sessions {
			if session.SessionID != "session-e2e-1" {
				continue
			}
			return session.EventCount == 2 &&
				session.TotalTokensIn == 23 &&
				session.TotalTokensOut == 10 &&
				session.TotalCostUSD > 0.0011 &&
				session.TotalCostUSD < 0.0013
		}
		return false
	})

	var activity activityResponse
	waitForCondition(t, ctx, "activity summary", func() error {
		return getJSON(client, ts.URL+"/api/activity?api_key="+activeKey+"&hours=24", &activity)
	}, func() bool {
		return activity.Summary.EventCount == 5 &&
			activity.Summary.AgentEvents == 2 &&
			activity.Summary.TotalTokensIn == 23 &&
			activity.Summary.TotalTokensOut == 10 &&
			activity.Summary.TotalCostUSD > 0.0011 &&
			activity.Summary.TotalCostUSD < 0.0013 &&
			len(activity.Summary.EventCounts) > 0 &&
			len(activity.Summary.Timeline) > 0
	})

	var filteredActivity activityResponse
	getJSONMust(t, client, ts.URL+"/api/activity?api_key="+activeKey+"&hours=24&event_type=agent", &filteredActivity)
	if filteredActivity.Summary.EventCount != 2 ||
		filteredActivity.Summary.AgentEvents != 2 ||
		filteredActivity.Summary.TotalTokensIn != 23 ||
		filteredActivity.Summary.TotalTokensOut != 10 {
		t.Fatalf("filtered activity ignored event_type: %+v", filteredActivity.Summary)
	}

	var userActivity activityResponse
	getJSONMust(t, client, ts.URL+"/api/activity?api_key="+activeKey+"&hours=24&distinct_id=user-e2e-1", &userActivity)
	if userActivity.Summary.EventCount != 5 {
		t.Fatalf("filtered activity ignored distinct_id: %+v", userActivity.Summary)
	}

	var trend insightResponse
	getJSONMust(t, client, ts.URL+"/api/insights/run?api_key="+activeKey+"&type=trend&hours=24", &trend)
	if trend.Insight.Type != "trend" || len(trend.Insight.Series) == 0 {
		t.Fatalf("trend insight did not return a series: %+v", trend.Insight)
	}

	var funnel insightResponse
	getJSONMust(t, client, ts.URL+"/api/insights/run?api_key="+activeKey+"&type=funnel&steps=user.pageview,user.conversion&hours=24", &funnel)
	if len(funnel.Insight.Funnel) != 2 || funnel.Insight.Funnel[0].Users == 0 {
		t.Fatalf("funnel insight did not return expected steps: %+v", funnel.Insight.Funnel)
	}

	var agentInsight insightResponse
	getJSONMust(t, client, ts.URL+"/api/insights/run?api_key="+activeKey+"&type=agent&hours=24", &agentInsight)
	if len(agentInsight.Insight.Rows) == 0 {
		t.Fatalf("agent insight returned no rows")
	}

	var retention insightResponse
	getJSONMust(t, client, ts.URL+"/api/insights/run?api_key="+activeKey+"&type=retention&metric=user.pageview&hours=24", &retention)
	if retention.Insight.Type != "retention" || len(retention.Insight.Retention) == 0 {
		t.Fatalf("unexpected retention insight: %+v", retention.Insight)
	}

	var tableInsight insightResponse
	getJSONMust(t, client, ts.URL+"/api/insights/run?api_key="+activeKey+"&type=table&hours=24&limit=10", &tableInsight)
	if len(tableInsight.Insight.Rows) == 0 {
		t.Fatalf("table insight returned no rows")
	}

	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "user.pageview",
		"distinct_id": "googlebot-e2e",
		"properties": map[string]any{
			"path":        "/ai-entry",
			"$referrer":   "https://chatgpt.com/c/abc",
			"$user_agent": "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		},
	})

	var webAnalytics webAnalyticsResponse
	waitForCondition(t, ctx, "web analytics provider split", func() error {
		return getJSON(client, ts.URL+"/api/web-analytics?api_key="+activeKey+"&hours=24", &webAnalytics)
	}, func() bool {
		if webAnalytics.WebAnalytics.Pageviews != 2 ||
			webAnalytics.WebAnalytics.Conversions != 1 ||
			len(webAnalytics.WebAnalytics.TopPaths) == 0 ||
			len(webAnalytics.WebAnalytics.AITopPaths) == 0 {
			return false
		}
		for _, row := range webAnalytics.WebAnalytics.TrafficByProvider {
			if row.Class == "search-bot" && row.Provider == "googlebot" && row.Pageviews == 1 {
				return true
			}
		}
		return false
	})

	var explorer explorerResponse
	getJSONMust(t, client, ts.URL+"/api/events/explore?api_key="+activeKey+"&session_id=session-e2e-1&limit=20", &explorer)
	if len(explorer.Explorer.Events) != 2 || len(explorer.Explorer.Timeline) != 2 {
		t.Fatalf("unexpected explorer result: %+v", explorer.Explorer)
	}

	var unfocusedExplorer explorerResponse
	getJSONMust(t, client, ts.URL+"/api/events/explore?api_key="+activeKey+"&limit=20", &unfocusedExplorer)
	if unfocusedExplorer.Explorer.Timeline == nil {
		t.Fatal("unfocused explorer timeline should serialize as an empty array, not null")
	}

	var replay replayResponse
	getJSONMust(t, client, ts.URL+"/api/sessions/session-e2e-1/replay?api_key="+activeKey, &replay)
	if replay.Replay.EventCount != 2 || replay.Replay.TotalTokensIn != 23 || replay.Replay.TotalTokensOut != 10 || len(replay.Replay.Events) != 2 {
		t.Fatalf("unexpected replay: %+v", replay.Replay)
	}

	var oldReplay replayResponse
	waitForCondition(t, ctx, "old replay", func() error {
		return getJSON(client, ts.URL+"/api/sessions/"+oldSessionID+"/replay?api_key="+activeKey, &oldReplay)
	}, func() bool {
		return oldReplay.Replay.EventCount == 1 &&
			oldReplay.Replay.TotalTokensIn == 5 &&
			oldReplay.Replay.TotalTokensOut == 7 &&
			len(oldReplay.Replay.Events) == 1
	})

	var saved savedQueryResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/saved-queries?api_key="+activeKey, map[string]any{
		"natural_language": "Events by type",
		"generated_sql":    "SELECT event_type, count() AS events FROM events WHERE project_id = {project_id} GROUP BY event_type",
		"verified":         true,
	}, &saved, http.StatusCreated)
	if saved.SavedQuery.ID == "" {
		t.Fatalf("saved query missing id: %+v", saved.SavedQuery)
	}

	var savedRun savedQueryRunResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/saved-queries/"+saved.SavedQuery.ID+"/run?api_key="+activeKey, map[string]any{}, &savedRun, http.StatusOK)
	if len(savedRun.Result.Rows) == 0 {
		t.Fatalf("saved query returned no rows")
	}

	var createdDashboard dashboardResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/dashboards?api_key="+activeKey, map[string]any{
		"name":        "Agent ops",
		"description": "Agent usage and cost",
	}, &createdDashboard, http.StatusCreated)
	if createdDashboard.Dashboard.ID == "" {
		t.Fatalf("created dashboard missing id: %+v", createdDashboard.Dashboard)
	}

	var updatedDashboard dashboardResponse
	requestJSON(t, client, http.MethodPut, ts.URL+"/api/dashboards/"+createdDashboard.Dashboard.ID+"?api_key="+activeKey, map[string]any{
		"name":        "Agent operations",
		"description": "Live agent usage and cost",
	}, &updatedDashboard, http.StatusOK)
	if updatedDashboard.Dashboard.Name != "Agent operations" {
		t.Fatalf("dashboard was not updated: %+v", updatedDashboard.Dashboard)
	}

	var dashboards dashboardsResponse
	getJSONMust(t, client, ts.URL+"/api/dashboards?api_key="+activeKey, &dashboards)
	hasCreatedDashboard := false
	for _, dashboard := range dashboards.Dashboards {
		if dashboard.ID == createdDashboard.Dashboard.ID {
			hasCreatedDashboard = true
			break
		}
	}
	if !hasCreatedDashboard {
		t.Fatalf("created dashboard missing from list: %+v", dashboards.Dashboards)
	}

	var createdChart chartResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/dashboards/"+createdDashboard.Dashboard.ID+"/charts?api_key="+activeKey, map[string]any{
		"name":       "Tool calls",
		"kind":       "bar",
		"metric":     "event_breakdown",
		"event_name": "agent.tool_call",
		"event_type": "agent",
	}, &createdChart, http.StatusCreated)
	if createdChart.Chart.ID == "" {
		t.Fatalf("created chart missing id: %+v", createdChart.Chart)
	}

	var updatedChart chartResponse
	requestJSON(t, client, http.MethodPut, ts.URL+"/api/charts/"+createdChart.Chart.ID+"?api_key="+activeKey, map[string]any{
		"name":       "Agent events",
		"kind":       "line",
		"metric":     "events",
		"event_name": "agent.tool_result",
		"event_type": "agent",
	}, &updatedChart, http.StatusOK)
	if updatedChart.Chart.Name != "Agent events" || updatedChart.Chart.Kind != "line" {
		t.Fatalf("chart was not updated: %+v", updatedChart.Chart)
	}

	var charts chartsResponse
	getJSONMust(t, client, ts.URL+"/api/dashboards/"+createdDashboard.Dashboard.ID+"/charts?api_key="+activeKey, &charts)
	if len(charts.Charts) != 1 || charts.Charts[0].Name != "Agent events" {
		t.Fatalf("unexpected chart list: %+v", charts.Charts)
	}

	assertStatus(t, client, http.MethodDelete, ts.URL+"/api/charts/"+createdChart.Chart.ID+"?api_key="+activeKey, nil, http.StatusNoContent)
	assertStatus(t, client, http.MethodDelete, ts.URL+"/api/dashboards/"+createdDashboard.Dashboard.ID+"?api_key="+activeKey, nil, http.StatusNoContent)

	var templates templatesResponse
	getJSONMust(t, client, ts.URL+"/api/templates?api_key="+activeKey, &templates)
	if len(templates.Templates) < 4 {
		t.Fatalf("expected dashboard templates, got %+v", templates.Templates)
	}

	// Find the ai-agent-ops template by name (ID is now a UUID from DB).
	var aiAgentOpsID string
	for _, tmpl := range templates.Templates {
		if tmpl.Name == "AI Agent Ops" {
			aiAgentOpsID = tmpl.ID
		}
	}
	if aiAgentOpsID == "" {
		t.Fatalf("AI Agent Ops template not found in %+v", templates.Templates)
	}

	var applied templateApplyResponse
	requestJSON(t, client, http.MethodPost, ts.URL+"/api/templates/"+aiAgentOpsID+"/apply?api_key="+activeKey, map[string]any{}, &applied, http.StatusCreated)
	if applied.Dashboard.ID == "" || len(applied.Charts) == 0 {
		t.Fatalf("template did not create dashboard/charts: %+v", applied)
	}
	if applied.Dashboard.Name != "AI Agent Ops" {
		t.Fatalf("expected dashboard name 'AI Agent Ops', got %q", applied.Dashboard.Name)
	}

	// ── Templates: all 4 system templates with correct chart counts ───────────
	t.Run("TestTemplatesFromDB", func(t *testing.T) {
		var tmplsResp templatesResponse
		getJSONMust(t, client, ts.URL+"/api/templates?api_key="+activeKey, &tmplsResp)
		want := map[string]int{
			"Product Overview": 8,
			"AI Agent Ops":     4,
			"Product Activity": 4,
			"Cost Control":     3,
		}
		got := map[string]int{}
		for _, tmpl := range tmplsResp.Templates {
			got[tmpl.Name] = len(tmpl.Charts)
		}
		for name, count := range want {
			if got[name] != count {
				t.Errorf("template %q: want %d charts, got %d", name, count, got[name])
			}
		}
	})

	// ── Templates: clone a single chart into a dashboard ─────────────────────
	t.Run("TestCloneTemplateChart", func(t *testing.T) {
		var tmplsResp templatesResponse
		getJSONMust(t, client, ts.URL+"/api/templates?api_key="+activeKey, &tmplsResp)

		var templateID, chartID string
		for _, tmpl := range tmplsResp.Templates {
			if tmpl.Name == "Product Overview" && len(tmpl.Charts) > 0 {
				templateID = tmpl.ID
				chartID = tmpl.Charts[0].ID
				break
			}
		}
		if templateID == "" || chartID == "" {
			t.Fatal("Product Overview template or chart not found")
		}

		// Create a target dashboard.
		type createDashResp struct {
			Dashboard struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"dashboard"`
		}
		var newDash createDashResp
		requestJSON(t, client, http.MethodPost, ts.URL+"/api/dashboards?api_key="+activeKey,
			map[string]any{"name": "Clone target", "description": ""}, &newDash, http.StatusCreated)
		if newDash.Dashboard.ID == "" {
			t.Fatal("failed to create target dashboard")
		}

		// Clone chart into that dashboard.
		type cloneChartResp struct {
			Chart struct {
				ID          string `json:"id"`
				DashboardID string `json:"dashboard_id"`
			} `json:"chart"`
		}
		var cloned cloneChartResp
		requestJSON(t, client, http.MethodPost,
			ts.URL+"/api/templates/"+templateID+"/charts/"+chartID+"/clone?api_key="+activeKey,
			map[string]any{"dashboard_id": newDash.Dashboard.ID}, &cloned, http.StatusCreated)
		if cloned.Chart.ID == "" || cloned.Chart.DashboardID != newDash.Dashboard.ID {
			t.Fatalf("cloned chart incorrect: %+v", cloned)
		}
	})

	// ── Templates: new project seeded from Product Overview template ──────────
	t.Run("TestSeedProjectFromTemplate", func(t *testing.T) {
		// Sign up a brand-new user to trigger SeedProjectFromTemplate.
		var authResp authResponse
		requestJSON(t, client, http.MethodPost, ts.URL+"/api/auth/signup",
			map[string]any{
				"email": "seed-test@example.com", "name": "Seed Tester",
				"password": "SeedTest1!", "workspace_name": "SeedWS", "project_name": "SeedProj",
			}, &authResp, http.StatusCreated)
		if len(authResp.Projects) == 0 {
			t.Fatal("signup did not return a project")
		}
		seedKey := authResp.Projects[0].APIKey

		type dashListResp struct {
			Dashboards []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"dashboards"`
		}
		var dashList dashListResp
		getJSONMust(t, client, ts.URL+"/api/dashboards?api_key="+seedKey, &dashList)
		if len(dashList.Dashboards) == 0 {
			t.Fatal("new project has no dashboards")
		}

		// Verify the seeded dashboard has at least 2 charts.
		type chartListResp struct {
			Charts []struct {
				ID string `json:"id"`
			} `json:"charts"`
		}
		var chartList chartListResp
		getJSONMust(t, client, ts.URL+"/api/dashboards/"+dashList.Dashboards[0].ID+"/charts?api_key="+seedKey, &chartList)
		if len(chartList.Charts) < 2 {
			t.Fatalf("seeded dashboard has %d charts, want >= 2", len(chartList.Charts))
		}
	})

	// ── Alias: anonymous → identified ────────────────────────────────────────
	// Send two events under an anonymous ID, then alias it to a new user and
	// verify the Persons endpoint merges them into a single entry.
	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "user.pageview",
		"distinct_id": "anon-pre-login",
		"properties":  map[string]any{"path": "/landing"},
	})
	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "user.pageview",
		"distinct_id": "anon-pre-login",
		"properties":  map[string]any{"path": "/pricing"},
	})

	// Alias: anonymous session belongs to the identified user.
	postJSON(t, client, ts.URL+"/alias", map[string]any{
		"api_key":      activeKey,
		"anonymous_id": "anon-pre-login",
		"distinct_id":  "user-after-login",
	})

	// Idempotent re-alias must not error.
	postJSON(t, client, ts.URL+"/alias", map[string]any{
		"api_key":      activeKey,
		"anonymous_id": "anon-pre-login",
		"distinct_id":  "user-after-login",
	})

	// Identify the canonical user.
	postJSON(t, client, ts.URL+"/identify", map[string]any{
		"api_key":     activeKey,
		"distinct_id": "user-after-login",
		"$set":        map[string]any{"email": "merged@example.com"},
	})
	postJSON(t, client, ts.URL+"/capture", map[string]any{
		"api_key":     activeKey,
		"event":       "user.pageview",
		"distinct_id": "user-after-login",
		"properties":  map[string]any{"path": "/dashboard"},
	})

	var persons personsResponse
	waitForCondition(t, ctx, "alias persons merge", func() error {
		return getJSON(client, ts.URL+"/api/persons?api_key="+activeKey, &persons)
	}, func() bool {
		// anon-pre-login must not appear as a separate person.
		for _, p := range persons.Persons.Persons {
			if p.DistinctID == "anon-pre-login" {
				return false
			}
		}
		// user-after-login must appear with combined event count (2 anon + 1 identify + 1 post-login).
		for _, p := range persons.Persons.Persons {
			if p.DistinctID == "user-after-login" && p.EventCount >= 4 {
				return true
			}
		}
		return false
	})

	var workspaceUsage workspaceUsageResponse
	getJSONMust(t, client, ts.URL+"/api/workspaces/"+signup.Workspaces[0].ID+"/usage?hours=24", &workspaceUsage)
	if workspaceUsage.Usage.ProjectCount < 2 || workspaceUsage.Usage.EventCount < 4 || workspaceUsage.Usage.DistinctUsers == 0 {
		t.Fatalf("workspace usage did not include workspace projects/events: %+v", workspaceUsage.Usage)
	}

	var aliasActivity activityResponse
	getJSONMust(t, client, ts.URL+"/api/activity?api_key="+activeKey+"&hours=24&distinct_id=user-after-login", &aliasActivity)
	if aliasActivity.Summary.EventCount < 4 || aliasActivity.Summary.DistinctUsers != 1 {
		t.Fatalf("aliased activity filter did not include anonymous history: %+v", aliasActivity.Summary)
	}

	var aliasWeb webAnalyticsResponse
	getJSONMust(t, client, ts.URL+"/api/web-analytics?api_key="+activeKey+"&hours=24&distinct_id=user-after-login", &aliasWeb)
	if aliasWeb.WebAnalytics.Visitors != 1 || aliasWeb.WebAnalytics.Pageviews < 3 {
		t.Fatalf("aliased web analytics did not canonicalize visitors: %+v", aliasWeb.WebAnalytics)
	}

	var aliasExplorer explorerResponse
	getJSONMust(t, client, ts.URL+"/api/events/explore?api_key="+activeKey+"&hours=24&distinct_id=user-after-login&limit=20", &aliasExplorer)
	hasRawAnonEvent := false
	for _, event := range aliasExplorer.Explorer.Events {
		if event.DistinctID == "anon-pre-login" {
			hasRawAnonEvent = true
			break
		}
	}
	if !hasRawAnonEvent {
		t.Fatalf("aliased explorer filter should return raw anonymous events: %+v", aliasExplorer.Explorer.Events)
	}

	assertStatus(t, client, http.MethodPost, ts.URL+"/api/auth/logout", nil, http.StatusNoContent)
	assertStatus(t, client, http.MethodGet, ts.URL+"/api/auth/me", nil, http.StatusUnauthorized)
}

func openRedis(rawURL string) (*redis.Client, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(options), nil
}

func serviceRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForTCP(t *testing.T, ctx context.Context, addr string) {
	t.Helper()
	waitForCondition(t, ctx, "tcp "+addr, func() error {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}, func() bool {
		return true
	})
}

func waitForHTTP(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	waitForCondition(t, ctx, "http "+url, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", res.StatusCode)
		}
		return nil
	}, func() bool {
		return true
	})
}

func waitForCondition(t *testing.T, ctx context.Context, name string, probe func() error, done func() bool) {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil && done() {
			return
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				t.Fatalf("%s: %v", name, lastErr)
			}
			t.Fatalf("%s: timed out waiting for condition", name)
		case <-ticker.C:
		}
	}
}

func assertStatus(t *testing.T, client *http.Client, method string, url string, body []byte, want int) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, url, res.StatusCode, want, body)
	}
}

func postJSON(t *testing.T, client *http.Client, url string, payload any) {
	t.Helper()
	requestJSON(t, client, http.MethodPost, url, payload, nil, http.StatusOK)
}

func requestJSON(t *testing.T, client *http.Client, method string, url string, payload any, out any, want int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, url, res.StatusCode, want, body)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, url, err)
		}
	}
}

func getJSON(client *http.Client, url string, out any) error {
	res, err := client.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status=%d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func getJSONMust(t *testing.T, client *http.Client, url string, out any) {
	t.Helper()
	if err := getJSON(client, url, out); err != nil {
		t.Fatal(err)
	}
}

func auditHasAction(audit workspaceAuditLogsResponse, action string) bool {
	for _, log := range audit.Logs {
		if log.Action == action {
			return true
		}
	}
	return false
}
