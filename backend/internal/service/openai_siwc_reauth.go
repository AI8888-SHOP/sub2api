package service

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/siwc"
)

type siwcTaskClaimer interface {
	ClaimNextSiwcTask(context.Context, string, time.Duration) (*OpenAIOAuthReauthTaskRecord, error)
}

// SIWC has its own Go consumer. Neither the Python worker nor a configured
// Session Studio service may claim these accounts or receive their passwords.
func (s *OpenAIOAuthReauthService) runSiwcWorker(ctx context.Context) {
	claimer, ok := s.repo.(siwcTaskClaimer)
	if !ok {
		return
	}
	workerID, err := openai.GenerateSessionID()
	if err != nil {
		return
	}
	workerID = "siwc-go-" + workerID
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			taskCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			record, err := claimer.ClaimNextSiwcTask(taskCtx, workerID, openAIOAuthReauthStaleAfter)
			if err == nil && record != nil {
				s.runSiwcReauth(taskCtx, record)
			}
			cancel()
		}
	}
}
func (s *OpenAIOAuthReauthService) runSiwcReauth(ctx context.Context, record *OpenAIOAuthReauthTaskRecord) {
	fail := func(reason string) {
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.repo.MarkFailed(failCtx, record.ID, record.WorkerID, reason)
	}
	account, err := s.accountFor(ctx, record.AccountID)
	if err != nil || !account.IsOpenAISiwc() {
		fail("SIWC account is unavailable")
		return
	}
	config, err := s.repo.GetConfig(ctx, account.ID)
	if err != nil || config == nil || config.CredentialMode != OpenAIOAuthReauthModePasswordTOTP {
		fail("SIWC password/TOTP configuration is unavailable")
		return
	}
	password, err := s.decryptReauthSecret(config.PasswordCiphertext, "SIWC password unavailable")
	if err != nil {
		fail("SIWC password unavailable")
		return
	}
	totp := ""
	if config.TOTPSecretCiphertext != "" {
		totp, err = s.decryptReauthSecret(config.TOTPSecretCiphertext, "SIWC TOTP unavailable")
		if err != nil {
			fail("SIWC TOTP unavailable")
			return
		}
	}
	var proxyID *int64
	switch config.ProxySource {
	case "", OpenAIOAuthReauthProxySourceAccount:
		proxyID = account.ProxyID
	case OpenAIOAuthReauthProxySourceManagedProxy:
		proxyID = config.ProxyID
	default:
		fail("SIWC Go login requires account or managed proxy")
		return
	}
	proxyURL, err := s.reauthProxyURL(ctx, proxyID)
	if err != nil {
		fail("SIWC proxy unavailable")
		return
	}
	previous := siwc.FromMap(account.Credentials)
	auth, err := s.oauth.siwcState().client.Start(proxyURL, &previous)
	if err != nil {
		fail("SIWC authorization unavailable")
		return
	}
	if err = s.repo.SetSession(ctx, record.ID, record.WorkerID, auth.SessionID); err != nil {
		s.oauth.siwcState().client.Cancel(auth.SessionID)
		fail("SIWC session could not be saved")
		return
	}
	callback, err := siwc.Login(ctx, auth.AuthURL, proxyURL, siwc.LoginInput{Email: config.LoginEmail, Password: password, TOTPSecret: totp})
	if err != nil {
		s.oauth.siwcState().client.Cancel(auth.SessionID)
		fail("SIWC requires browser confirmation: edit this account and choose SIWC reauthorization")
		return
	}
	if _, err = s.SubmitCallback(ctx, record.ID, record.WorkerID, callback); err != nil {
		fail("SIWC authorization failed; reauthorize from the account editor")
	}
}
func (s *OpenAIOAuthService) GenerateSiwcReconnect(ctx context.Context, account *Account, login *SiwcLoginInput) (*siwc.Authorization, error) {
	if account == nil || !account.IsOpenAISiwc() {
		return nil, errors.New("account is not SIWC")
	}
	return s.generateSiwcAuth(ctx, account.ProxyID, login, account)
}
