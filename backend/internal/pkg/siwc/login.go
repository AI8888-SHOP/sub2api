package siwc

// The authorization UI is not part of OpenClaw's OAuth client contract. This
// bounded, ordinary HTTP login adapter handles observed password/TOTP endpoints
// and server-rendered consent forms. JS challenges, unknown SPA consent and
// additional verification return ErrManual, with no proof/token fabrication.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
)

var ErrManual = errors.New("SIWC requires browser confirmation")

type LoginInput struct{ Email, Password, TOTPSecret string }

func TOTP(secret string, now time.Time) (string, error) {
	secret = strings.ToUpper(strings.Join(strings.Fields(secret), ""))
	secret = strings.TrimRight(secret, "=")
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if e != nil || len(key) < 10 || len(key) > 80 {
		return "", errors.New("invalid TOTP secret")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1000000), nil
}
func authTarget(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host != "auth.openai.com" || u.User != nil || u.Fragment != "" {
		return nil, ErrManual
	}
	return u, nil
}
func Login(ctx context.Context, authURL, proxy string, input LoginInput) (string, error) {
	return LoginWithProgress(ctx, authURL, proxy, input, nil)
}

// Progress emits fixed, secret-free stage labels; no URLs, bodies or cookies.
func LoginWithProgress(ctx context.Context, authURL, proxy string, input LoginInput, progress func(string)) (string, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if proxy != "" {
		p, e := url.Parse(proxy)
		if e != nil {
			return "", ErrManual
		}
		tr.Proxy = http.ProxyURL(p)
	}
	defer tr.CloseIdleConnections()
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	client := &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return loginWithClientProgress(ctx, client, authURL, input, progress)
}
func loginWithClient(ctx context.Context, client *http.Client, authURL string, input LoginInput) (string, error) {
	return loginWithClientProgress(ctx, client, authURL, input, nil)
}
func loginWithClientProgress(ctx context.Context, client *http.Client, authURL string, input LoginInput, progress func(string)) (string, error) {
	auth, e := authTarget(authURL)
	if e != nil || auth.Path != "/api/accounts/authorize" || auth.Query().Get("redirect_uri") != Redirect || auth.Query().Get("resource") != Resource || auth.Query().Get("state") == "" {
		return "", ErrInvalid
	}
	current := authURL
	method := "GET"
	var body []byte
	contentType := ""
	referer := Issuer + "/"
	seen := map[string]bool{}
	for step := 0; step < 24; step++ {
		u, err := url.Parse(current)
		if err != nil {
			return "", ErrManual
		}
		if u.Scheme == "http" && u.Host == "localhost:8080" && u.Path == "/auth/callback" && u.User == nil && u.Fragment == "" && u.Query().Get("state") == auth.Query().Get("state") {
			return current, nil
		}
		if _, err = authTarget(current); err != nil {
			return "", ErrManual
		}
		req, _ := http.NewRequestWithContext(ctx, method, current, bytes.NewReader(body))
		req.Header.Set("User-Agent", "node")
		req.Header.Set("Accept", "application/json, text/html")
		req.Header.Set("Referer", referer)
		if method == "POST" {
			req.Header.Set("Origin", Issuer)
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", ErrManual
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
		_ = resp.Body.Close()
		if readErr != nil || len(raw) > 2<<20 {
			return "", ErrManual
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			target, err := u.Parse(resp.Header.Get("Location"))
			if err != nil || resp.Header.Get("Location") == "" {
				return "", ErrManual
			}
			// Never replay a credential POST through a 307/308 redirect.
			if method == "POST" && (resp.StatusCode == 307 || resp.StatusCode == 308) {
				return "", ErrManual
			}
			referer = current
			current = target.String()
			method = "GET"
			body = nil
			continue
		}
		if progress != nil {
			stage := "authorization"
			if seen["email"] {
				stage = "email"
			}
			if seen["password"] {
				stage = "password"
			}
			if seen["totp"] {
				stage = "totp"
			}
			progress(fmt.Sprintf("%s: HTTP %d", stage, resp.StatusCode))
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", ErrManual
		}
		lower := strings.ToLower(string(raw))
		if strings.Contains(lower, "cf-chl-") || strings.Contains(lower, "verify you are human") || strings.Contains(lower, "captcha") || strings.Contains(lower, "sentinel_required") {
			return "", ErrManual
		}
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		next, _ := data["continue_url"].(string)
		// Handle TOTP challenge only when the server identifies an existing TOTP factor.
		if session, ok := data["oai-client-auth-session"].(map[string]any); ok {
			factor := ""
			for _, field := range []string{"mfa_challenge_factors", "mfa_factors"} {
				if factors, ok := session[field].([]any); ok {
					for _, f := range factors {
						if m, ok := f.(map[string]any); ok && m["factor_type"] == "totp" {
							factor, _ = m["id"].(string)
							break
						}
					}
				}
			}
			if factor != "" && !seen["totp"] {
				if _, err := TOTP(input.TOTPSecret, time.Now()); err != nil {
					return "", ErrManual
				}
				seen["totp"] = true
				for _, op := range []struct {
					path    string
					payload map[string]any
				}{{"/api/accounts/mfa/issue_challenge", map[string]any{"type": "totp", "id": factor, "force_fresh_challenge": false}}, {"/api/accounts/mfa/verify", map[string]any{"type": "totp", "id": factor}}} {
					if op.path == "/api/accounts/mfa/verify" {
						code, err := TOTP(input.TOTPSecret, time.Now())
						if err != nil {
							return "", ErrManual
						}
						op.payload["code"] = code
					}
					payload, _ := json.Marshal(op.payload)
					request, _ := http.NewRequestWithContext(ctx, "POST", Issuer+op.path, bytes.NewReader(payload))
					request.Header.Set("User-Agent", "node")
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Origin", Issuer)
					request.Header.Set("Referer", current)
					response, err := client.Do(request)
					if err != nil {
						return "", ErrManual
					}
					b, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
					_ = response.Body.Close()
					if err != nil || response.StatusCode != 200 {
						return "", ErrManual
					}
					if op.path == "/api/accounts/mfa/verify" {
						var v map[string]any
						if json.Unmarshal(b, &v) != nil {
							return "", ErrManual
						}
						next, _ = v["continue_url"].(string)
					}
				}
			}
		}
		if next != "" {
			target, err := u.Parse(next)
			if err != nil {
				return "", ErrManual
			}
			referer = current
			current = target.String()
			method = "GET"
			body = nil
			continue
		}
		// Prefer real form actions, hidden CSRF values and named controls supplied by
		// the auth server. Unknown fields/pages are left for browser confirmation.
		target, values, stage, ok := loginForm(raw, u, input)
		if ok {
			key := target + "|" + stage
			if seen[key] {
				return "", ErrManual
			}
			seen[key] = true
			referer = current
			current = target
			method = "POST"
			contentType = "application/x-www-form-urlencoded"
			body = []byte(values.Encode())
			continue
		}
		if (u.Path == "/log-in" || u.Path == "/log-in/email") && !seen["email"] {
			seen["email"] = true
			referer = current
			current = Issuer + "/api/accounts/authorize/continue"
			method = "POST"
			contentType = "application/json"
			body, _ = json.Marshal(map[string]any{"username": map[string]any{"kind": "email", "value": input.Email}})
			continue
		}
		// Observed account password endpoint. Security challenge failures deliberately
		// stop here instead of generating browser/Sentinel proofs.
		if strings.HasPrefix(u.Path, "/log-in/password") && !seen["password"] {
			seen["password"] = true
			referer = current
			current = Issuer + "/api/accounts/password/verify"
			method = "POST"
			contentType = "application/json"
			body, _ = json.Marshal(map[string]any{"password": input.Password})
			continue
		}
		return "", ErrManual
	}
	return "", ErrManual
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}
func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeText(c))
	}
	return b.String()
}
func loginForm(raw []byte, base *url.URL, input LoginInput) (string, url.Values, string, bool) {
	root, e := html.Parse(bytes.NewReader(raw))
	if e != nil {
		return "", nil, "", false
	}
	var forms []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" {
			forms = append(forms, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	for _, form := range forms {
		if !strings.EqualFold(attr(form, "method"), "post") {
			continue
		}
		target, e := base.Parse(attr(form, "action"))
		if e != nil {
			continue
		}
		if _, e = authTarget(target.String()); e != nil {
			continue
		}
		values := url.Values{}
		stage := ""
		unknown := false
		sharing := false
		consent := strings.Contains(strings.ToLower(nodeText(form)), "openclaw")
		var submitters []*html.Node
		var controls func(*html.Node)
		controls = func(n *html.Node) {
			if n.Type == html.ElementNode && (hasAttr(n, "disabled") || n.Data == "select" || n.Data == "textarea") {
				if n.Data == "select" || n.Data == "textarea" {
					unknown = true
				}
				return
			}
			if n.Type == html.ElementNode && (n.Data == "input" || n.Data == "button") {
				name := attr(n, "name")
				kind := strings.ToLower(attr(n, "type"))
				if kind == "submit" || (n.Data == "button" && kind == "") {
					submitters = append(submitters, n)
					return
				}
				if name != "" {
					switch kind {
					case "hidden":
						values.Add(name, attr(n, "value"))
					case "email":
						values.Set(name, input.Email)
						stage = "email"
					case "password":
						values.Set(name, input.Password)
						stage = "password"
					case "", "text", "tel", "number":
						if n.Data == "button" {
							break
						} else if name == "email" || name == "username" {
							values.Set(name, input.Email)
							stage = "email"
						} else if attr(n, "autocomplete") == "one-time-code" && strings.Contains(strings.ToLower(nodeText(form)), "authenticator") {
							code, e := TOTP(input.TOTPSecret, time.Now())
							if e != nil {
								unknown = true
							} else {
								values.Set(name, code)
								stage = "totp"
							}
						} else {
							unknown = true
						}
					case "checkbox":
						if consent && (strings.Contains(name, "sharing") || strings.Contains(name, "allowance") || strings.Contains(name, "tokens")) {
							v := attr(n, "value")
							if v == "" {
								v = "on"
							}
							values.Set(name, v)
							sharing = true
						} else {
							unknown = true
						}
					default:
						unknown = true
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				controls(c)
			}
		}
		controls(form)
		if consent && sharing {
			stage = "consent"
		}
		var chosen *html.Node
		for _, button := range submitters {
			if stage == "consent" {
				value := strings.ToLower(strings.TrimSpace(attr(button, "value")))
				label := strings.ToLower(strings.TrimSpace(nodeText(button)))
				if value != "" && value != "allow" && value != "approve" && value != "accept" {
					continue
				}
				if value == "" && label != "allow" && label != "approve" && label != "accept" {
					continue
				}
			} else if len(submitters) != 1 {
				unknown = true
				break
			}
			if chosen != nil || hasAttr(button, "formaction") || hasAttr(button, "formmethod") {
				unknown = true
				break
			}
			chosen = button
		}
		if stage == "consent" && chosen == nil {
			unknown = true
		}
		if chosen != nil && attr(chosen, "name") != "" {
			values.Set(attr(chosen, "name"), attr(chosen, "value"))
		}
		if !unknown && stage != "" {
			return target.String(), values, stage, true
		}
	}
	return "", nil, "", false
}
