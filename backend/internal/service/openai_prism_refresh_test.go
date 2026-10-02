package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func prismRefreshFixture(t *testing.T, responseStatus int, customBody ...string) (*OpenAIOAuthReauthService, *reauthTestAccountReader, *reauthTestUpdater) {
	t.Helper()
	responseBody := `{"prism":{"prism_cookie":"prism_session_token=fresh","prism_session_user_id":"user-1","prism_session_email":"user@example.com","prism_template":{"headers":{"Cookie":"prism_session_token=fresh"}}}}`
	if len(customBody) > 0 {
		responseBody = customBody[0]
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/login", r.URL.Path)
		require.Equal(t, "Bearer fixture-worker-token", r.Header.Get("Authorization"))
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, "user@example.com", input["email"])
		require.Equal(t, "password-secret", input["password"])
		require.Equal(t, "totp-secret", input["mfa_secret"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(responseStatus)
		if responseStatus == http.StatusOK {
			_, _ = w.Write([]byte(responseBody))
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("PRISM_LOGIN_WORKER_URL", server.URL+"/login")
	t.Setenv("PRISM_LOGIN_WORKER_TOKEN", "fixture-worker-token")
	svc, reader, _, updater, _, _ := newReauthTestService("acct-1")
	reader.account.Extra = map[string]any{"openai_prism_browser": true}
	reader.account.Credentials[PrismCookieKey] = "prism_session_token=anonymous"
	reader.account.Credentials["prism_old_field"] = "stale"
	savePasswordReauthConfig(t, svc)
	return svc, reader, updater
}

func waitPrismRefresh(t *testing.T, svc *OpenAIOAuthReauthService, accountID int64, id string) *OpenAIPrismRefreshJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, found := svc.GetPrismRefresh(accountID, id)
		require.True(t, found)
		if job.Status != OpenAIPrismRefreshRunning {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Prism refresh did not complete")
	return nil
}

func TestOpenAIPrismRefreshReplacesOnlyPrismCredentials(t *testing.T) {
	svc, _, updater := prismRefreshFixture(t, http.StatusOK)
	job, err := svc.StartPrismRefresh(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, OpenAIPrismRefreshRunning, job.Status)
	finished := waitPrismRefresh(t, svc, 42, job.ID)
	require.Equal(t, OpenAIPrismRefreshSucceeded, finished.Status)
	require.Empty(t, finished.ErrorCode)
	require.True(t, updater.applied)
	require.Equal(t, "prism_session_token=fresh", updater.credentials[PrismCookieKey])
	require.NotContains(t, updater.credentials, "prism_session_user_id")
	require.NotContains(t, updater.credentials, "prism_session_email")
	require.NotContains(t, updater.credentials, "prism_old_field")
	require.Equal(t, "old-access", updater.credentials["access_token"])
	require.Equal(t, "preserved", updater.credentials["custom_setting"])
	_, found := svc.GetPrismRefresh(59, job.ID)
	require.False(t, found)
}

func TestOpenAIPrismRefreshRequiresMatchingSessionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"different identity", `{"prism":{"prism_cookie":"prism_session_token=fresh","prism_session_user_id":"another-user","prism_session_email":"other@example.com","prism_template":{}}}`},
		{"missing identity", `{"prism":{"prism_cookie":"prism_session_token=fresh","prism_template":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, updater := prismRefreshFixture(t, http.StatusOK, tc.body)
			job, err := svc.StartPrismRefresh(context.Background(), 42)
			require.NoError(t, err)
			finished := waitPrismRefresh(t, svc, 42, job.ID)
			require.Equal(t, OpenAIPrismRefreshFailed, finished.Status)
			require.Equal(t, "prism_identity_mismatch", finished.ErrorCode)
			require.False(t, updater.applied)
		})
	}
}

func TestOpenAIPrismRefreshAcceptsMatchingSessionEmail(t *testing.T) {
	body := `{"prism":{"prism_cookie":"prism_session_token=fresh","prism_session_user_id":"another-user","prism_session_email":"USER@EXAMPLE.COM","prism_template":{}}}`
	svc, _, updater := prismRefreshFixture(t, http.StatusOK, body)
	job, err := svc.StartPrismRefresh(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, OpenAIPrismRefreshSucceeded, waitPrismRefresh(t, svc, 42, job.ID).Status)
	require.True(t, updater.applied)
}

func TestOpenAIPrismRefreshFailureDoesNotWriteOrReportSuccess(t *testing.T) {
	svc, _, updater := prismRefreshFixture(t, http.StatusUnprocessableEntity)
	job, err := svc.StartPrismRefresh(context.Background(), 42)
	require.NoError(t, err)
	finished := waitPrismRefresh(t, svc, 42, job.ID)
	require.Equal(t, OpenAIPrismRefreshFailed, finished.Status)
	require.Equal(t, "prism_capture_failed", finished.ErrorCode)
	require.False(t, updater.applied)
}

func TestOpenAIPrismRefreshRejectsConcurrentAccountEdit(t *testing.T) {
	svc, _, updater := prismRefreshFixture(t, http.StatusOK)
	updater.prismCASMiss = true
	job, err := svc.StartPrismRefresh(context.Background(), 42)
	require.NoError(t, err)
	finished := waitPrismRefresh(t, svc, 42, job.ID)
	require.Equal(t, OpenAIPrismRefreshFailed, finished.Status)
	require.Equal(t, "account_changed", finished.ErrorCode)
	require.False(t, updater.applied)
}

func TestOpenAIPrismRefreshRejectsDuplicateRunningJob(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})
	t.Setenv("PRISM_LOGIN_WORKER_URL", server.URL+"/login")
	t.Setenv("PRISM_LOGIN_WORKER_TOKEN", "fixture-worker-token")
	svc, reader, _, _, _, _ := newReauthTestService("acct-1")
	reader.account.Extra = map[string]any{"openai_prism_browser": true}
	savePasswordReauthConfig(t, svc)
	job, err := svc.StartPrismRefresh(context.Background(), 42)
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Prism worker request did not start")
	}
	_, err = svc.StartPrismRefresh(context.Background(), 42)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	releaseOnce.Do(func() { close(release) })
	require.Equal(t, OpenAIPrismRefreshFailed, waitPrismRefresh(t, svc, 42, job.ID).Status)
}

func TestOpenAIPrismRefreshRequiresEnabledAccountAndSavedCredentials(t *testing.T) {
	svc, reader, repo, _, _, _ := newReauthTestService("acct-1")
	t.Setenv("PRISM_LOGIN_WORKER_TOKEN", "fixture-worker-token")
	_, err := svc.StartPrismRefresh(context.Background(), 42)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	reader.account.Extra = map[string]any{"openai_prism_browser": true}
	_, err = svc.StartPrismRefresh(context.Background(), 42)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	savePasswordReauthConfig(t, svc)
	repo.config.TOTPSecretCiphertext = ""
	_, err = svc.StartPrismRefresh(context.Background(), 42)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	_, found := svc.GetPrismRefresh(42, "missing")
	require.False(t, found)
}

func TestPrismWorkerFailureOnlyMapsFixedSafeCodes(t *testing.T) {
	for _, tc := range []struct {
		body string
		code string
	}{
		{`{"error":{"code":"prism_login_failed","message":"prism_auth_required"}}`, "prism_auth_required"},
		{`{"error":{"code":"prism_login_failed","message":"openai_login_failed"}}`, "openai_login_failed"},
		{`{"error":{"code":"prism_login_failed","message":"password=secret"}}`, ""},
		{`{"error":{"code":"other","message":"prism_auth_required"}}`, ""},
	} {
		err := safePrismWorkerFailure(strings.NewReader(tc.body))
		if tc.code == "" {
			require.NoError(t, err)
		} else {
			require.EqualError(t, err, tc.code)
		}
	}
}
