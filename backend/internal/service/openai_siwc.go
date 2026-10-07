package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/siwc"
)

// Initial passwords live only in the Go login job. Durable relogin secrets use
// the existing encrypted token-guard configuration, never account credentials.
type SiwcLoginInput struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
}
type SiwcLoginStatus struct {
	Status      string         `json:"status"`
	Message     string         `json:"message,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
}
type siwcJob struct {
	status  SiwcLoginStatus
	created time.Time
	cancel  context.CancelFunc
	expiry  *time.Timer
}
type siwcServiceState struct {
	client *siwc.Client
	mu     sync.Mutex
	jobs   map[string]*siwcJob
	closed bool
	wg     sync.WaitGroup
}

func (s *OpenAIOAuthService) siwcState() *siwcServiceState {
	s.siwcOnce.Do(func() { s.siwc = &siwcServiceState{client: siwc.New(), jobs: map[string]*siwcJob{}} })
	return s.siwc
}
func (s *OpenAIOAuthService) GenerateSiwcAuth(ctx context.Context, proxyID *int64, login *SiwcLoginInput) (*siwc.Authorization, error) {
	return s.generateSiwcAuth(ctx, proxyID, login, nil)
}
func (s *OpenAIOAuthService) generateSiwcAuth(ctx context.Context, proxyID *int64, login *SiwcLoginInput, account *Account) (*siwc.Authorization, error) {
	if login != nil && (login.Email == "" || login.Password == "" || len(login.Email) > 320 || len(login.Password) > 4096 || len(login.TOTPSecret) > 256) {
		return nil, errors.New("invalid SIWC login credentials")
	}
	proxyURL := ""
	if proxyID != nil {
		if s.proxyRepo == nil {
			return nil, errors.New("proxy repository unavailable")
		}
		p, e := s.proxyRepo.GetByID(ctx, *proxyID)
		if e != nil {
			return nil, e
		}
		if p != nil {
			proxyURL = p.URL()
		}
	}
	state := s.siwcState()
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return nil, errors.New("SIWC service has stopped")
	}
	for id, job := range state.jobs {
		if time.Since(job.created) > 30*time.Minute {
			if job.cancel != nil {
				job.cancel()
			}
			if job.expiry != nil {
				job.expiry.Stop()
			}
			delete(state.jobs, id)
			state.client.Cancel(id)
		}
	}
	if len(state.jobs) >= 16 {
		state.mu.Unlock()
		return nil, errors.New("too many SIWC login sessions")
	}
	var previous *siwc.Credential
	if account != nil {
		c := siwc.FromMap(account.Credentials)
		previous = &c
	}
	auth, e := state.client.Start(proxyURL, previous)
	if e != nil {
		state.mu.Unlock()
		return nil, e
	}
	job := &siwcJob{status: SiwcLoginStatus{Status: "waiting_callback"}, created: time.Now()}
	state.jobs[auth.SessionID] = job
	job.expiry = time.AfterFunc(30*time.Minute, func() { s.CancelSiwcLogin(auth.SessionID) })
	if login != nil && login.Email != "" && login.Password != "" {
		runCtx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		job.cancel = cancel
		job.status.Status = "running"
		credentials := *login
		state.wg.Add(1)
		go func() {
			defer state.wg.Done()
			defer cancel()
			callback, err := siwc.Login(runCtx, auth.AuthURL, proxyURL, siwc.LoginInput{Email: credentials.Email, Password: credentials.Password, TOTPSecret: credentials.TOTPSecret})
			credentials = SiwcLoginInput{}
			state.mu.Lock()
			current, ok := state.jobs[auth.SessionID]
			if !ok || current != job || job.status.Status != "running" {
				state.mu.Unlock()
				return
			}
			if err != nil {
				job.status = SiwcLoginStatus{Status: "requires_manual", Message: "Automatic login could not finish. Open the authorization URL and paste the full callback, or start a new session."}
				state.mu.Unlock()
				return
			}
			job.status.Status = "exchanging"
			state.mu.Unlock()
			cred, err := state.client.Exchange(runCtx, auth.SessionID, callback)
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.jobs[auth.SessionID] != job || job.status.Status != "exchanging" {
				return
			}
			if err != nil {
				job.status = SiwcLoginStatus{Status: "requires_manual", Message: "Token exchange failed. Cancel and start a new SIWC authorization session."}
				return
			}
			job.status = SiwcLoginStatus{Status: "succeeded", Credentials: cred.Map()}
		}()
	}
	state.mu.Unlock()
	return auth, nil
}
func (s *OpenAIOAuthService) SiwcLoginStatus(id string) (SiwcLoginStatus, error) {
	state := s.siwcState()
	state.mu.Lock()
	defer state.mu.Unlock()
	job, ok := state.jobs[id]
	if !ok || time.Since(job.created) > 30*time.Minute {
		return SiwcLoginStatus{}, errors.New("SIWC login expired")
	}
	return job.status, nil
}
func (s *OpenAIOAuthService) CancelSiwcLogin(id string) {
	state := s.siwcState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if job := state.jobs[id]; job != nil && job.cancel != nil {
		job.cancel()
	}
	if job := state.jobs[id]; job != nil && job.expiry != nil {
		job.expiry.Stop()
	}
	delete(state.jobs, id)
	state.client.Cancel(id)
}
func (s *OpenAIOAuthService) ExchangeSiwcCallback(ctx context.Context, id, callback string) (*OpenAITokenInfo, error) {
	state := s.siwcState()
	state.mu.Lock()
	job := state.jobs[id]
	if job == nil || (job.status.Status != "running" && job.status.Status != "waiting_callback" && job.status.Status != "requires_manual") {
		state.mu.Unlock()
		return nil, errors.New("SIWC login is expired or already exchanging")
	}
	job.status.Status = "exchanging"
	if job.cancel != nil {
		job.cancel()
	}
	state.mu.Unlock()
	cred, e := state.client.Exchange(ctx, id, callback)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.jobs[id] != job {
		return nil, errors.New("SIWC login was cancelled")
	}
	if e != nil {
		job.status = SiwcLoginStatus{Status: "requires_manual", Message: "Check the complete callback URL. If token exchange was attempted, cancel and start a new session."}
		return nil, e
	}
	job.status = SiwcLoginStatus{Status: "succeeded", Credentials: cred.Map()}
	return siwcTokenInfo(cred), nil
}
func siwcTokenInfo(c *siwc.Credential) *OpenAITokenInfo {
	expiry, _ := time.Parse(time.RFC3339, c.ExpiresAt)
	return &OpenAITokenInfo{AccessToken: c.Access, RefreshToken: c.Refresh, IDToken: c.IDToken, ClientID: c.ClientID, AuthMode: "siwc", Email: c.Email, ExpiresAt: expiry.Unix(), ExpiresIn: int64(time.Until(expiry).Seconds()), Siwc: c}
}
func (a *Account) IsOpenAISiwc() bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth && (a.GetCredential("auth_mode") == "siwc" || a.GetCredential("auth_flow") == siwc.Sharing || a.GetCredential("auth_flow") == siwc.Identity)
}
func (a *Account) HasSiwcSharing() bool {
	return a.IsOpenAISiwc() && a.GetCredential("auth_flow") == siwc.Sharing && siwc.HasSharing(a.GetCredential("granted_scope"))
}
