package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"time"
)

// Login jobs keep token results in bounded, short-lived memory. Successful logins
// save the supplied password/2FA to the guard without enabling it or creating accounts.
type OpenAITwoFALoginJob struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Credential map[string]any `json:"credential,omitempty"`
	cancel     context.CancelFunc
	expiry     *time.Timer
}

type prismWorkerFailure struct{ code string }

func (e *prismWorkerFailure) Error() string { return e.code }

func safePrismWorkerFailure(resp io.Reader) error {
	var result struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp, 16<<10)).Decode(&result) != nil || result.Error.Code != "prism_login_failed" {
		return nil
	}
	switch result.Error.Message {
	case "openai_login_failed", "prism_auth_required", "invalid_mfa_secret", "prism_cookie_not_found", "prism_metadata_not_found":
		return &prismWorkerFailure{code: result.Error.Message}
	default:
		return nil
	}
}

func ValidateOpenAITwoFALogin(entry AccountTokenGuardReloginAccount) error {
	email := strings.TrimSpace(entry.Email)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 320 ||
		strings.TrimSpace(entry.Password) == "" || len(entry.Password) > 4096 ||
		strings.TrimSpace(entry.MFASecret) == "" || len(entry.MFASecret) > 4096 {
		return errors.New("请填写有效的邮箱、密码和 2FA 密钥")
	}
	return nil
}

func (s *AccountTokenGuardService) StartTwoFALogin(ctx context.Context, entry AccountTokenGuardReloginAccount) (*OpenAITwoFALoginJob, error) {
	return s.startTwoFALogin(ctx, entry, true, false)
}

// Operations imports save their login method through the encrypted per-account
// API after identity deduplication returns the account ID. Do not also enroll
// them in the legacy guard's plaintext settings.
func (s *AccountTokenGuardService) StartTwoFALoginForOperations(ctx context.Context, entry AccountTokenGuardReloginAccount) (*OpenAITwoFALoginJob, error) {
	return s.startTwoFALogin(ctx, entry, false, false)
}

// StartTwoFALoginForOperationsPrism performs the normal OAuth login and then
// asks the isolated Playwright worker to capture the Prism browser session.
// The worker is opt-in because it needs a separate Chromium container.
func (s *AccountTokenGuardService) StartTwoFALoginForOperationsPrism(ctx context.Context, entry AccountTokenGuardReloginAccount) (*OpenAITwoFALoginJob, error) {
	return s.startTwoFALogin(ctx, entry, false, true)
}

func (s *AccountTokenGuardService) startTwoFALogin(ctx context.Context, entry AccountTokenGuardReloginAccount, saveToLegacyGuard, includePrism bool) (*OpenAITwoFALoginJob, error) {
	if err := ValidateOpenAITwoFALogin(entry); err != nil {
		return nil, err
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, errors.New("无法读取凭证守护配置")
	}
	// Explicit login works while scheduled inspection/automatic relogin is off.
	if err := validateGuardHTTPURL(cfg.ReloginEndpoint, "relogin_endpoint"); err != nil {
		return nil, errors.New("请在凭证守护中配置有效的重登接口")
	}
	entry.Email = strings.ToLower(strings.TrimSpace(entry.Email))
	entry.MFASecret = strings.TrimSpace(entry.MFASecret)
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if len(s.logins) >= 32 {
		return nil, errors.New("登录任务过多，请稍后重试")
	}
	if s.logins == nil {
		s.logins = make(map[string]*OpenAITwoFALoginJob)
	}
	loginCtx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	job := &OpenAITwoFALoginJob{ID: guardRandomID(), Status: "running", cancel: cancel}
	s.logins[job.ID] = job
	job.expiry = time.AfterFunc(30*time.Minute, func() { s.DeleteTwoFALogin(job.ID) })
	go func() {
		defer cancel()
		credential, loginErr := s.relogin(loginCtx, cfg, entry)
		if loginErr == nil && includePrism {
			var prism map[string]any
			prism, loginErr = fetchPrismWorkerCredential(loginCtx, entry, credential)
			if loginErr == nil {
				for key, value := range prism {
					credential[key] = value
				}
			}
		}
		s.loginMu.Lock()
		defer s.loginMu.Unlock()
		if s.logins[job.ID] != job {
			return
		}
		if loginErr != nil {
			// Neither provider errors nor request data may be reflected to clients.
			job.Status = "failed"
			return
		}
		// Persist verified login credentials before the client imports tokens and
		// clears its password/MFA input. Never report success on a failed save.
		if saveToLegacyGuard {
			if err := s.saveTwoFALoginAccount(loginCtx, entry); err != nil {
				job.Status = "failed"
				return
			}
		}
		job.Credential = twoFALoginCredential(credential, entry.Email)
		job.Status = "succeeded"
	}()
	return &OpenAITwoFALoginJob{ID: job.ID, Status: job.Status}, nil
}

// Merge into the latest persisted config, not the snapshot from login start:
// logins can take minutes, and other imports/config edits may finish meanwhile.
func (s *AccountTokenGuardService) saveTwoFALoginAccount(ctx context.Context, entry AccountTokenGuardReloginAccount) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg, err := s.getConfigLocked(ctx)
	if err != nil {
		return err
	}
	// GetConfig also caches this slice; do not mutate the published snapshot.
	accounts := make([]AccountTokenGuardReloginAccount, 0, len(cfg.ReloginAccounts)+1)
	for _, existing := range cfg.ReloginAccounts {
		if existing.Email != entry.Email {
			accounts = append(accounts, existing)
		}
	}
	cfg.ReloginAccounts = append(accounts, entry)
	_, err = s.saveConfigLocked(ctx, cfg)
	return err
}

// Only token/session fields enter the existing Session importer. Discard any
// echoed password, MFA secret, or unexpected service configuration fields.
func twoFALoginCredential(in map[string]any, email string) map[string]any {
	out := map[string]any{"email": email}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "expired", "account_id", "chatgpt_account_id", "chatgpt_user_id", "user_id", "client_id", "plan_type", "prism_cookie", "prism_template", "prism_project_id", "prism_user_id", "prism_sandbox_url", "prism_sandbox_token"} {
		switch value := in[key].(type) {
		case string:
			out[key] = value
		case map[string]any:
			if key == "prism_template" {
				out[key] = value
			}
		case float64:
			if key == "expires_at" || key == "expired" {
				out[key] = value
			}
		}
	}
	return out
}

// fetchPrismWorkerCredential calls the internal Playwright service. It never
// logs the request or response because both contain account secrets.
func fetchPrismWorkerCredential(ctx context.Context, entry AccountTokenGuardReloginAccount, oauth map[string]any) (map[string]any, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("PRISM_LOGIN_WORKER_URL")), "/")
	if endpoint == "" {
		endpoint = "http://prism-worker:8090/login"
	}
	token := strings.TrimSpace(os.Getenv("PRISM_LOGIN_WORKER_TOKEN"))
	if token == "" {
		return nil, errors.New("Prism Playwright 服务未配置 PRISM_LOGIN_WORKER_TOKEN")
	}
	payload := map[string]any{
		"email": entry.Email, "password": entry.Password, "mfa_secret": entry.MFASecret,
		"access_token": guardText(oauth["access_token"]),
		"id_token":     guardText(oauth["id_token"]),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("Prism 采集请求构造失败")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Prism 采集地址无效")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Prism Playwright 服务不可用: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnprocessableEntity {
			if safeFailure := safePrismWorkerFailure(resp.Body); safeFailure != nil {
				return nil, safeFailure
			}
		}
		return nil, fmt.Errorf("Prism Playwright 服务返回 HTTP %d", resp.StatusCode)
	}
	var result struct {
		Prism map[string]any `json:"prism"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := decoder.Decode(&result); err != nil {
		return nil, errors.New("Prism 采集响应格式错误")
	}
	if result.Error != nil {
		return nil, errors.New("Prism 采集失败")
	}
	if strings.TrimSpace(guardText(result.Prism["prism_cookie"])) == "" {
		return nil, errors.New("Prism 采集未返回 Cookie")
	}
	if _, ok := result.Prism["prism_template"].(map[string]any); !ok {
		return nil, errors.New("Prism 采集未返回模板")
	}
	return result.Prism, nil
}

func (s *AccountTokenGuardService) TwoFALogin(id string) (*OpenAITwoFALoginJob, bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	job, ok := s.logins[id]
	if !ok {
		return nil, false
	}
	snapshot := &OpenAITwoFALoginJob{ID: job.ID, Status: job.Status}
	if job.Credential != nil {
		snapshot.Credential = make(map[string]any, len(job.Credential))
		for key, value := range job.Credential {
			snapshot.Credential[key] = value
		}
	}
	return snapshot, true
}

func (s *AccountTokenGuardService) DeleteTwoFALogin(id string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if job, ok := s.logins[id]; ok {
		job.cancel()
		job.expiry.Stop()
		job.Credential = nil
		delete(s.logins, id)
	}
}

func (s *AccountTokenGuardService) clearTwoFALogins() {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	for id, job := range s.logins {
		job.cancel()
		job.expiry.Stop()
		job.Credential = nil
		delete(s.logins, id)
	}
}
