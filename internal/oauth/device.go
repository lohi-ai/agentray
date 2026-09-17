package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/ai"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// codexDeviceTimeout bounds each device-flow HTTP call (the TS pins 15s).
const codexDeviceTimeout = 15 * time.Second

// errNoPending is returned by PollDeviceLogin for an unknown pending id.
var errNoPending = errors.New("unknown or expired device login")

// DeviceStart is StartDeviceLogin's result.
type DeviceStart struct {
	PendingID       string `json:"pending_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval_seconds"`
}

// DevicePoll is PollDeviceLogin's result. Status is "pending" while the user
// has not entered the code, "done" once the account is stored, "error" on a
// terminal failure.
type DevicePoll struct {
	Status  string                            `json:"status"`
	Message string                            `json:"message,omitempty"`
	Account *storage.WorkspaceProviderAccount `json:"account,omitempty"`
}

// StartDeviceLogin begins the Codex headless flow: POST the usercode request,
// record a pending entry keyed by a random pending id, and hand back the code
// the user enters at the verification URL.
func (m *Manager) StartDeviceLogin(ctx context.Context, userID, workspaceID, providerID string) (DeviceStart, error) {
	vendor, err := m.providerVendor(ctx, userID, workspaceID, providerID)
	if err != nil {
		return DeviceStart{}, err
	}
	if normalizeVendor(vendor) != ai.VendorOpenAICodex {
		return DeviceStart{}, &Error{Kind: "validation",
			Message: "provider " + vendor + " does not support device login"}
	}
	d, err := m.descriptorFor(ai.VendorOpenAICodex)
	if err != nil {
		return DeviceStart{}, err
	}
	return m.startDeviceLogin(ctx, d, workspaceID, providerID)
}

// startDeviceLogin POSTs the usercode request and records the pending entry.
// Split from StartDeviceLogin so tests skip the store-backed vendor lookup.
func (m *Manager) startDeviceLogin(ctx context.Context, d *providerDescriptor, workspaceID, providerID string) (DeviceStart, error) {
	ctx, cancel := context.WithTimeout(ctx, codexDeviceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.deviceUsercodeURL,
		strings.NewReader(fmt.Sprintf(`{"client_id":%q}`, d.clientID)))
	if err != nil {
		return DeviceStart{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return DeviceStart{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceStart{}, err
	}
	if resp.StatusCode/100 != 2 {
		return DeviceStart{}, &Error{Kind: "device-auth", Status: resp.StatusCode,
			Message: fmt.Sprintf("device authorization initiation failed: %d", resp.StatusCode)}
	}
	var init struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     any    `json:"interval"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return DeviceStart{}, &Error{Kind: "validation", Message: "device authorization response was not JSON"}
	}
	if init.DeviceAuthID == "" || init.UserCode == "" {
		return DeviceStart{}, &Error{Kind: "validation", Message: "device authorization response missing required fields"}
	}

	interval := 5
	switch v := init.Interval.(type) {
	case float64:
		if v > 0 {
			interval = int(v)
		}
	case string:
		if n, err := parseInt(v); err == nil && n > 0 {
			interval = n
		}
	}

	pendingID, err := randomURLSafe(16)
	if err != nil {
		return DeviceStart{}, err
	}
	m.putPending(pendingID, PendingLogin{
		Vendor:       ai.VendorOpenAICodex,
		WorkspaceID:  workspaceID,
		ProviderID:   providerID,
		DeviceAuthID: init.DeviceAuthID,
		UserCode:     init.UserCode,
		CreatedAt:    time.Now(),
	})
	return DeviceStart{
		PendingID:       pendingID,
		UserCode:        init.UserCode,
		VerificationURL: d.deviceVerifyURL,
		Interval:        interval,
	}, nil
}

// PollDeviceLogin makes exactly one poll attempt — the HTTP layer drives the
// polling cadence, not a goroutine here. 403/404 mean the user has not entered
// the code yet; a 200 carries the authorization_code + code_verifier which are
// exchanged at the token endpoint with the device redirect URI.
func (m *Manager) PollDeviceLogin(ctx context.Context, userID, workspaceID, providerID, pendingID string) (DevicePoll, error) {
	p, ok := m.getPending(pendingID)
	if !ok {
		return DevicePoll{Status: "error", Message: errNoPending.Error()}, errNoPending
	}
	if p.WorkspaceID != workspaceID || p.ProviderID != providerID {
		return DevicePoll{Status: "error", Message: "device login belongs to a different workspace or provider"}, errNoPending
	}
	d, err := m.descriptorFor(ai.VendorOpenAICodex)
	if err != nil {
		return DevicePoll{Status: "error", Message: err.Error()}, err
	}

	res, done, err := m.pollDeviceOnce(ctx, d, p)
	if err != nil {
		m.deletePending(pendingID)
		return DevicePoll{Status: "error", Message: err.Error()}, nil
	}
	if !done {
		return DevicePoll{Status: "pending"}, nil
	}

	acct, err := m.store.CreateProviderAccount(ctx, userID, workspaceID, p.ProviderID, storage.ProviderAccountInput{
		Email:        res.Email,
		AccountID:    res.AccountID,
		OrgID:        res.OrgID,
		OrgName:      res.OrgName,
		Plan:         res.Plan,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    res.ExpiresAt,
	})
	if err != nil {
		return DevicePoll{Status: "error", Message: err.Error()}, err
	}
	m.deletePending(pendingID)
	return DevicePoll{Status: "done", Account: &acct}, nil
}

// pollDeviceOnce performs the single deviceauth/token POST. done=false means
// still waiting on the user.
func (m *Manager) pollDeviceOnce(ctx context.Context, d *providerDescriptor, p PendingLogin) (TokenResult, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, codexDeviceTimeout)
	defer cancel()

	body := fmt.Sprintf(`{"device_auth_id":%q,"user_code":%q}`, p.DeviceAuthID, p.UserCode)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.deviceTokenURL, strings.NewReader(body))
	if err != nil {
		return TokenResult{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return TokenResult{}, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResult{}, false, err
	}
	// 403/404 = authorization pending.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return TokenResult{}, false, nil
	}
	if resp.StatusCode/100 != 2 {
		return TokenResult{}, false, &Error{Kind: "polling", Status: resp.StatusCode,
			Message: fmt.Sprintf("device token polling failed: %d", resp.StatusCode)}
	}
	var poll struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(raw, &poll); err != nil {
		return TokenResult{}, false, &Error{Kind: "validation", Message: "device token response was not JSON"}
	}
	if poll.AuthorizationCode == "" || poll.CodeVerifier == "" {
		return TokenResult{}, false, &Error{Kind: "validation",
			Message: "device token response missing authorization_code or code_verifier"}
	}

	res, err := m.exchangeCode(ctx, d, poll.AuthorizationCode, poll.CodeVerifier, d.deviceRedirectURI, "")
	if err != nil {
		return TokenResult{}, false, err
	}
	if err := m.enrichLogin(ctx, d, &res); err != nil {
		return TokenResult{}, false, err
	}
	return res, true, nil
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}
