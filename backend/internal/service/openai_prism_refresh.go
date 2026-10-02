package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OpenAIPrismRefreshRunning   = "running"
	OpenAIPrismRefreshSucceeded = "succeeded"
	OpenAIPrismRefreshFailed    = "failed"
)

// OpenAIPrismRefreshJob never contains login material or captured credentials.
type OpenAIPrismRefreshJob struct {
	ID        string `json:"id"`
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

func (s *OpenAIOAuthReauthService) StartPrismRefresh(ctx context.Context, accountID int64) (*OpenAIPrismRefreshJob, error) {
	if err := s.ensureReady(); err != nil {
		return nil, err
	}
	if err := s.ensureDurableEncryption(); err != nil {
		return nil, err
	}
	account, err := s.accountFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !account.IsPrismBrowserEnabled() {
		return nil, infraerrors.New(http.StatusBadRequest, "PRISM_REFRESH_NOT_ENABLED", "Prism Browser is not enabled for this account")
	}
	stored, err := s.repo.GetConfig(ctx, accountID)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "PRISM_REFRESH_CONFIG_READ_FAILED", "failed to read re-login configuration")
	}
	if stored == nil || stored.CredentialMode != OpenAIOAuthReauthModePasswordTOTP ||
		stored.PasswordCiphertext == "" || stored.TOTPSecretCiphertext == "" || stored.LoginEmail == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "PRISM_REFRESH_CREDENTIALS_REQUIRED", "saved password and TOTP credentials are required")
	}
	if strings.TrimSpace(os.Getenv("PRISM_LOGIN_WORKER_TOKEN")) == "" {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "PRISM_REFRESH_WORKER_UNAVAILABLE", "Prism login worker is not configured")
	}

	s.prismRefreshMu.Lock()
	defer s.prismRefreshMu.Unlock()
	if s.prismRefreshJobs == nil {
		s.prismRefreshJobs = make(map[int64]*OpenAIPrismRefreshJob)
	}
	if current := s.prismRefreshJobs[accountID]; current != nil && current.Status == OpenAIPrismRefreshRunning {
		return nil, infraerrors.New(http.StatusConflict, "PRISM_REFRESH_ALREADY_RUNNING", "Prism refresh is already running for this account")
	}
	job := &OpenAIPrismRefreshJob{ID: guardRandomID(), AccountID: accountID, Status: OpenAIPrismRefreshRunning}
	s.prismRefreshJobs[accountID] = job
	go s.runPrismRefresh(job, account, *stored)
	copy := *job
	return &copy, nil
}

func (s *OpenAIOAuthReauthService) GetPrismRefresh(accountID int64, jobID string) (*OpenAIPrismRefreshJob, bool) {
	s.prismRefreshMu.Lock()
	defer s.prismRefreshMu.Unlock()
	job := s.prismRefreshJobs[accountID]
	if job == nil || job.ID != jobID {
		return nil, false
	}
	copy := *job
	return &copy, true
}

func prismCredentialsOnly(credentials map[string]any) map[string]any {
	prism := make(map[string]any)
	for key, value := range credentials {
		if strings.HasPrefix(key, "prism_") {
			prism[key] = value
		}
	}
	return prism
}

func prismSessionMatchesAccount(prism map[string]any, account *Account, loginEmail string) bool {
	if account == nil {
		return false
	}
	sessionUserID := strings.TrimSpace(guardText(prism["prism_session_user_id"]))
	accountUserID := strings.TrimSpace(guardText(account.Credentials["chatgpt_user_id"]))
	if sessionUserID != "" && accountUserID != "" && subtle.ConstantTimeCompare([]byte(sessionUserID), []byte(accountUserID)) == 1 {
		return true
	}
	sessionEmail := strings.TrimSpace(guardText(prism["prism_session_email"]))
	loginEmail = strings.TrimSpace(loginEmail)
	return sessionEmail != "" && loginEmail != "" && strings.EqualFold(sessionEmail, loginEmail)
}

func (s *OpenAIOAuthReauthService) runPrismRefresh(job *OpenAIPrismRefreshJob, original *Account, stored OpenAIOAuthReauthStoredConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()
	code := s.capturePrismRefresh(ctx, original, stored)
	s.prismRefreshMu.Lock()
	if code == "" {
		job.Status = OpenAIPrismRefreshSucceeded
	} else {
		job.Status = OpenAIPrismRefreshFailed
		job.ErrorCode = code
	}
	s.prismRefreshMu.Unlock()
	time.AfterFunc(30*time.Minute, func() {
		s.prismRefreshMu.Lock()
		if s.prismRefreshJobs[job.AccountID] == job {
			delete(s.prismRefreshJobs, job.AccountID)
		}
		s.prismRefreshMu.Unlock()
	})
}

func (s *OpenAIOAuthReauthService) capturePrismRefresh(ctx context.Context, original *Account, stored OpenAIOAuthReauthStoredConfig) string {
	password, err := s.decryptReauthSecret(stored.PasswordCiphertext, "login password cannot be decrypted")
	if err != nil || password == "" {
		return "credential_decryption_failed"
	}
	mfaSecret, err := s.decryptReauthSecret(stored.TOTPSecretCiphertext, "TOTP secret cannot be decrypted")
	if err != nil || mfaSecret == "" {
		return "credential_decryption_failed"
	}
	entry := AccountTokenGuardReloginAccount{Email: stored.LoginEmail, Password: password, MFASecret: mfaSecret}
	prism, err := fetchPrismWorkerCredential(ctx, entry, original.Credentials)
	if err != nil {
		var workerFailure *prismWorkerFailure
		if errors.As(err, &workerFailure) {
			return workerFailure.code
		}
		return "prism_capture_failed"
	}
	if !prismSessionMatchesAccount(prism, original, stored.LoginEmail) {
		return "prism_identity_mismatch"
	}
	latestConfig, err := s.repo.GetConfig(ctx, original.ID)
	if err != nil || latestConfig == nil || latestConfig.PasswordCiphertext != stored.PasswordCiphertext ||
		latestConfig.TOTPSecretCiphertext != stored.TOTPSecretCiphertext || latestConfig.LoginEmail != stored.LoginEmail {
		return "credentials_changed"
	}
	latest, err := s.accountFor(ctx, original.ID)
	if err != nil || !latest.IsPrismBrowserEnabled() ||
		!reflect.DeepEqual(prismCredentialsOnly(original.Credentials), prismCredentialsOnly(latest.Credentials)) ||
		!reflect.DeepEqual(latest.Credentials["chatgpt_account_id"], original.Credentials["chatgpt_account_id"]) ||
		!reflect.DeepEqual(latest.Credentials["chatgpt_user_id"], original.Credentials["chatgpt_user_id"]) {
		return "account_changed"
	}
	merged := make(map[string]any, len(latest.Credentials)+len(prism))
	for key, value := range latest.Credentials {
		if !strings.HasPrefix(key, "prism_") {
			merged[key] = value
		}
	}
	for key, value := range prism {
		if strings.HasPrefix(key, "prism_") && key != "prism_session_user_id" && key != "prism_session_email" {
			merged[key] = value
		}
	}
	if _, ok := merged[PrismCookieKey].(string); !ok {
		return "prism_capture_failed"
	}
	changed, err := s.credentialUpdater.ApplyOpenAIPrismRefresh(ctx, latest.ID, latest.Credentials, merged)
	if err != nil {
		return "account_update_failed"
	}
	if !changed {
		return "account_changed"
	}
	if s.tokenCacheInvalidator != nil {
		updated := *latest
		updated.Credentials = merged
		_ = s.tokenCacheInvalidator.InvalidateToken(ctx, &updated)
	}
	return ""
}
