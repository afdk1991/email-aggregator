package security

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPKCE_Challenge(t *testing.T) {
	pkce := NewPKCE()
	sum := sha256.Sum256([]byte(pkce.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != want {
		t.Fatalf("challenge mismatch: got %q want %q", pkce.Challenge, want)
	}
	if pkce.Method != "S256" {
		t.Fatalf("method = %q want S256", pkce.Method)
	}
	if len(pkce.Verifier) < 43 {
		t.Fatalf("verifier too short: %q", pkce.Verifier)
	}
}

func TestOAuth2_AuthCodeURL(t *testing.T) {
	cfg := &OAuth2Config{ClientID: "cid", AuthURL: "https://auth.example.com/authorize", RedirectURI: "https://app/cb", Scopes: []string{"mail.read", "offline_access"}}
	pkce := NewPKCE()
	u := cfg.AuthCodeURL("state-xyz", pkce)
	if !strings.Contains(u, "code_challenge="+pkce.Challenge) {
		t.Fatalf("missing code_challenge: %s", u)
	}
	if !strings.Contains(u, "code_challenge_method=S256") {
		t.Fatalf("missing method: %s", u)
	}
	if !strings.Contains(u, "state=state-xyz") {
		t.Fatalf("missing state: %s", u)
	}
	if !strings.Contains(u, "scope=mail.read+offline_access") {
		t.Fatalf("missing scope: %s", u)
	}
}

func newTokenMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-123","refresh_token":"rt-123","expires_in":3600,"token_type":"Bearer"}`))
	}))
}

// TestOAuth2_Exchange 验证授权码兑换：PKCE verifier 随请求发出，解析出 access/refresh/expiresAt。
func TestOAuth2_Exchange(t *testing.T) {
	srv := newTokenMock(t)
	defer srv.Close()

	cfg := &OAuth2Config{ClientID: "cid", TokenURL: srv.URL, Client: srv.Client()}
	pkce := NewPKCE()
	tok, err := cfg.Exchange(context.Background(), "auth-code", pkce)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.AccessToken != "at-123" || tok.RefreshToken != "rt-123" {
		t.Fatalf("token mismatch: %+v", tok)
	}
	if tok.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt not in future: %d", tok.ExpiresAt)
	}
}

// TestOAuth2_Refresh 验证 refresh token 换新 access token。
func TestOAuth2_Refresh(t *testing.T) {
	srv := newTokenMock(t)
	defer srv.Close()

	cfg := &OAuth2Config{ClientID: "cid", TokenURL: srv.URL, Client: srv.Client()}
	tok, err := cfg.Refresh(context.Background(), "rt-123")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken != "at-123" {
		t.Fatalf("refreshed access token mismatch: %q", tok.AccessToken)
	}
}

// TestOAuth2_Exchange_SendsVerifier 验证请求体确实携带 grant_type 与 code_verifier。
func TestOAuth2_Exchange_SendsVerifier(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":1}`))
	}))
	defer srv.Close()

	cfg := &OAuth2Config{ClientID: "cid", ClientSecret: "sec", TokenURL: srv.URL, Client: srv.Client()}
	pkce := NewPKCE()
	if _, err := cfg.Exchange(context.Background(), "code", pkce); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if gotForm.Get("code_verifier") != pkce.Verifier {
		t.Fatalf("code_verifier mismatch")
	}
	if gotForm.Get("client_secret") != "sec" {
		t.Fatalf("client_secret not sent")
	}
}
