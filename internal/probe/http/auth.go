package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Auth is the `auth:` block. Exactly one method may be set, and every secret
// can come from a file instead of the configuration.
type Auth struct {
	Basic  *BasicAuth  `yaml:"basic,omitempty"`
	Bearer *BearerAuth `yaml:"bearer,omitempty"`
	OAuth2 *OAuth2     `yaml:"oauth2,omitempty"`
}

type BasicAuth struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password,omitempty"`
	PasswordFile string `yaml:"password_file,omitempty"`
}

type BearerAuth struct {
	Token     string `yaml:"token,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
}

// OAuth2 is the client-credentials flow.
type OAuth2 struct {
	ClientID         string            `yaml:"client_id"`
	ClientSecret     string            `yaml:"client_secret,omitempty"`
	ClientSecretFile string            `yaml:"client_secret_file,omitempty"`
	TokenURL         string            `yaml:"token_url"`
	Scopes           []string          `yaml:"scopes,omitempty"`
	Params           map[string]string `yaml:"params,omitempty"`

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (a *Auth) validate() error {
	set := 0
	for _, on := range []bool{a.Basic != nil, a.Bearer != nil, a.OAuth2 != nil} {
		if on {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("auth: set one of basic, bearer, oauth2")
	}
	switch {
	case a.Basic != nil:
		if a.Basic.Username == "" {
			return fmt.Errorf("auth.basic.username is required")
		}
		return exclusive("auth.basic", a.Basic.Password, a.Basic.PasswordFile)
	case a.Bearer != nil:
		return exclusive("auth.bearer", a.Bearer.Token, a.Bearer.TokenFile)
	case a.OAuth2 != nil:
		if a.OAuth2.ClientID == "" || a.OAuth2.TokenURL == "" {
			return fmt.Errorf("auth.oauth2: client_id and token_url are required")
		}
		return exclusive("auth.oauth2", a.OAuth2.ClientSecret, a.OAuth2.ClientSecretFile)
	}
	return nil
}

func exclusive(prefix, value, file string) error {
	if value != "" && file != "" {
		return fmt.Errorf("%s: set the value or the file, not both", prefix)
	}
	return nil
}

// apply attaches credentials; a token endpoint is never fetched with the pinned client.
func (a *Auth) apply(ctx context.Context, req *http.Request, dialer *net.Dialer, timeout time.Duration) error {
	switch {
	case a == nil:
		return nil
	case a.Basic != nil:
		password, err := secret(a.Basic.Password, a.Basic.PasswordFile)
		if err != nil {
			return err
		}
		req.SetBasicAuth(a.Basic.Username, password)
	case a.Bearer != nil:
		token, err := secret(a.Bearer.Token, a.Bearer.TokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	case a.OAuth2 != nil:
		token, err := a.OAuth2.get(ctx, dialer, timeout)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// secret reads the value or the file it lives in, re-reading the file each time.
func secret(value, file string) (string, error) {
	if file == "" {
		return value, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading secret: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// tokenClient is a single-request client bound to the node's source address.
func tokenClient(dialer *net.Dialer, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true},
	}
}

// get returns a cached token, fetching a new one shortly before the old expires.
func (o *OAuth2) get(ctx context.Context, dialer *net.Dialer, timeout time.Duration) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" && time.Now().Before(o.expires) {
		return o.token, nil
	}
	client := tokenClient(dialer, timeout)

	secretValue, err := secret(o.ClientSecret, o.ClientSecretFile)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(o.Scopes) > 0 {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	for k, v := range o.Params {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(o.ClientID, secretValue)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth2: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("oauth2: token endpoint returned %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("oauth2: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("oauth2: no access_token in the response")
	}

	o.token = body.AccessToken
	lifetime := time.Duration(body.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	// Renew early so a token cannot expire mid-check.
	o.expires = time.Now().Add(lifetime - lifetime/10)
	return o.token, nil
}
