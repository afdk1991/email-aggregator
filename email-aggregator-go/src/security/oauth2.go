package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"email-aggregator-go/src/model"
)

// OAuth2Config OAuth2 客户端配置（Authorization Code + PKCE 流程，RFC6749 + RFC7636）。
// 适用于 Gmail / Outlook / Exchange 等支持 OAuth2 的邮箱服务商，
// 产出的 model.OAuthToken 可直接喂给 IMAP(XOAUTH2) / EWS(Bearer) 连接器。
type OAuth2Config struct {
	ClientID     string
	ClientSecret string
	AuthURL      string // 授权端点，如 https://accounts.google.com/o/oauth2/v2/auth
	TokenURL     string // 令牌端点，如 https://oauth2.googleapis.com/token
	RedirectURI  string
	Scopes       []string
	Client       *http.Client // 可选注入（测试用 httptest）
}

func (c *OAuth2Config) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// PKCE 证明密钥码交换参数（RFC7636）。
type PKCE struct {
	Verifier  string // 43-128 字符的随机码（base64url，无 padding）
	Challenge string // S256: base64url(sha256(Verifier))
	Method    string // "S256"
}

// NewPKCE 生成 PKCE 挑战对。
func NewPKCE() PKCE {
	b := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, b)
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge, Method: "S256"}
}

// AuthCodeURL 构造授权跳转 URL（携带 code_challenge 与 state，防 CSRF）。
func (c *OAuth2Config) AuthCodeURL(state string, pkce PKCE) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	if c.RedirectURI != "" {
		q.Set("redirect_uri", c.RedirectURI)
	}
	if len(c.Scopes) > 0 {
		q.Set("scope", strings.Join(c.Scopes, " "))
	}
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	return c.AuthURL + "?" + q.Encode()
}

// Exchange 用授权码兑换令牌（携带 PKCE verifier）。返回 model.OAuthToken（含 access/refresh/expiresAt）。
func (c *OAuth2Config) Exchange(ctx context.Context, code string, pkce PKCE) (*model.OAuthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURI)
	form.Set("client_id", c.ClientID)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	form.Set("code_verifier", pkce.Verifier)
	return c.doToken(ctx, form)
}

// Refresh 用 refresh token 换新 access token（不依赖 PKCE）。
func (c *OAuth2Config) Refresh(ctx context.Context, refreshToken string) (*model.OAuthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.ClientID)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	return c.doToken(ctx, form)
}

// doToken 向令牌端点 POST 并解析响应为 model.OAuthToken。
func (c *OAuth2Config) doToken(ctx context.Context, form url.Values) (*model.OAuthToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("oauth2 token status=%d body=%s", resp.StatusCode, truncateStrSec(data, 200))
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("oauth2 token parse: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("oauth2: empty access_token")
	}
	return &model.OAuthToken{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix(),
	}, nil
}

func truncateStrSec(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}
