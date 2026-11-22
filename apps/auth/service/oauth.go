package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/computeflux/auth/config"
)

// Profile 社交登录拉取到的用户资料。
type Profile struct {
	Provider string // google / github / x
	Subject  string // provider 的 user id
	Email    string
	Name     string
	Avatar   string
	WalletAddr string // 钱包地址（wallet 登录时 = Subject）
}

// OAuth 社交登录：code(+PKCE) 换 IdP token → 拉取资料。
type OAuth struct {
	cfg *config.Config
}

func NewOAuth(cfg *config.Config) *OAuth { return &OAuth{cfg: cfg} }

// Exchange 用 authorization code 换 IdP token 并拉取资料。
// redirectURI 必须与前端发起授权时一致（SDK 在 socialLogin 里回传）。
func (o *OAuth) Exchange(provider, code, codeVerifier, redirectURI string) (*Profile, error) {
	switch provider {
	case "google":
		return o.exchangeGoogle(code, redirectURI)
	case "github":
		return o.exchangeGitHub(code, redirectURI)
	case "x", "twitter":
		return o.exchangeX(code, codeVerifier, redirectURI)
	default:
		return nil, fmt.Errorf("provider %q not supported", provider)
	}
}

func postForm(client *http.Client, endpoint string, form url.Values, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bad response from %s (http %d): %s", endpoint, resp.StatusCode, truncate(string(body), 200))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, endpoint, truncate(string(body), 200))
	}
	return out, nil
}

func getJSON(client *http.Client, endpoint string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, endpoint, truncate(string(body), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bad response from %s: %s", endpoint, truncate(string(body), 200))
	}
	return out, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (o *OAuth) exchangeGoogle(code, redirectURI string) (*Profile, error) {
	if o.cfg.AuthGoogleClientID == "" || o.cfg.AuthGoogleClientSecret == "" {
		return nil, fmt.Errorf("google oauth not configured (AUTH_GOOGLE_CLIENT_ID/SECRET)")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	form := url.Values{
		"code":          {code},
		"client_id":     {o.cfg.AuthGoogleClientID},
		"client_secret": {o.cfg.AuthGoogleClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	tok, err := postForm(client, "https://oauth2.googleapis.com/token", form, nil)
	if err != nil {
		return nil, err
	}
	accessToken := str(tok["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("google token exchange returned no access_token")
	}
	info, err := getJSON(client, "https://www.googleapis.com/oauth2/v2/userinfo",
		map[string]string{"Authorization": "Bearer " + accessToken})
	if err != nil {
		return nil, err
	}
	return &Profile{
		Provider: "google",
		Subject:  str(info["id"]),
		Email:    str(info["email"]),
		Name:     str(info["name"]),
		Avatar:   str(info["picture"]),
	}, nil
}

func (o *OAuth) exchangeGitHub(code, redirectURI string) (*Profile, error) {
	if o.cfg.AuthGitHubClientID == "" || o.cfg.AuthGitHubClientSecret == "" {
		return nil, fmt.Errorf("github oauth not configured (AUTH_GITHUB_CLIENT_ID/SECRET)")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	form := url.Values{
		"client_id":     {o.cfg.AuthGitHubClientID},
		"client_secret": {o.cfg.AuthGitHubClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	tok, err := postForm(client, "https://github.com/login/oauth/access_token", form, nil)
	if err != nil {
		return nil, err
	}
	accessToken := str(tok["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("github token exchange returned no access_token")
	}
	hdrs := map[string]string{"Authorization": "Bearer " + accessToken}
	user, err := getJSON(client, "https://api.github.com/user", hdrs)
	if err != nil {
		return nil, err
	}
	p := &Profile{Provider: "github", Subject: str(user["login"]), Name: str(user["name"]), Avatar: str(user["avatar_url"])}
	if p.Subject == "" {
		// login 缺失时退回 numeric id
		if id, ok := user["id"].(float64); ok {
			p.Subject = fmt.Sprintf("%d", int64(id))
		}
	}
	// 主邮箱（user 接口不保证返回 email）
	if emails, eerr := getJSON(client, "https://api.github.com/user/emails", hdrs); eerr == nil {
		if list, ok := emails["emails"].([]any); ok {
			for _, e := range list {
				if m, ok := e.(map[string]any); ok && str(m["primary"]) == "true" {
					p.Email = str(m["email"])
					break
				}
			}
		}
	}
	return p, nil
}

func (o *OAuth) exchangeX(code, codeVerifier, redirectURI string) (*Profile, error) {
	if o.cfg.AuthXClientID == "" || o.cfg.AuthXClientSecret == "" {
		return nil, fmt.Errorf("x oauth not configured (AUTH_X_CLIENT_ID/SECRET)")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	form := url.Values{
		"code":          {code},
		"client_id":     {o.cfg.AuthXClientID},
		"client_secret": {o.cfg.AuthXClientSecret},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	tok, err := postForm(client, "https://api.x.com/2/oauth2/token", form, nil)
	if err != nil {
		return nil, err
	}
	accessToken := str(tok["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("x token exchange returned no access_token")
	}
	me, err := getJSON(client, "https://api.x.com/2/users/me?user.fields=id,name,profile_image_url",
		map[string]string{"Authorization": "Bearer " + accessToken})
	if err != nil {
		return nil, err
	}
	data, _ := me["data"].(map[string]any)
	if data == nil {
		return nil, fmt.Errorf("x /users/me returned no data")
	}
	return &Profile{
		Provider: "x",
		Subject:  str(data["id"]),
		Name:     str(data["name"]),
		Avatar:   str(data["profile_image_url"]),
	}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
