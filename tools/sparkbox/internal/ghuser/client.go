// Package ghuser manages repository-scoped GitHub App user access tokens.
// Unlike installation tokens, requests made with these credentials are
// attributed to the user who authorized the App.
package ghuser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultCodeURL      = "https://github.com/login/device/code"
	defaultAuthorizeURL = "https://github.com/login/oauth/authorize"
	defaultTokenURL     = "https://github.com/login/oauth/access_token"
	defaultAPIURL       = "https://api.github.com"
)

var (
	ErrPending    = errors.New("github authorization is pending")
	ErrSlowDown   = errors.New("github requested slower polling")
	ErrDenied     = errors.New("github authorization was declined")
	ErrExpired    = errors.New("github authorization expired")
	ErrBadRefresh = errors.New("github refresh token is invalid or expired")
	ErrWrongUser  = errors.New("github authorization belongs to a different user")
	ErrWrongScope = errors.New("github authorization is not restricted to the requested repository")
	ErrNoRefresh  = errors.New("github app did not issue an expiring user token with a refresh token")
)

type Config struct {
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client
	CodeURL      string
	AuthorizeURL string
	TokenURL     string
	APIURL       string
	Now          func() time.Time
}

type Client struct {
	clientID     string
	clientSecret string
	hc           *http.Client
	codeURL      string
	authorizeURL string
	tokenURL     string
	apiURL       string
	now          func() time.Time
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("github user authorization needs an app client id")
	}
	c := &Client{clientID: strings.TrimSpace(cfg.ClientID), clientSecret: strings.TrimSpace(cfg.ClientSecret), hc: cfg.HTTPClient,
		codeURL: cfg.CodeURL, authorizeURL: cfg.AuthorizeURL, tokenURL: cfg.TokenURL, apiURL: strings.TrimSuffix(cfg.APIURL, "/"), now: cfg.Now}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if c.codeURL == "" {
		c.codeURL = defaultCodeURL
	}
	if c.authorizeURL == "" {
		c.authorizeURL = defaultAuthorizeURL
	}
	if c.tokenURL == "" {
		c.tokenURL = defaultTokenURL
	}
	if c.apiURL == "" {
		c.apiURL = defaultAPIURL
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

func (c *Client) WebEnabled() bool { return c.clientSecret != "" }

func (c *Client) AuthorizationURL(redirectURI, state, challenge string) (string, error) {
	if !c.WebEnabled() {
		return "", errors.New("github web authorization needs an app client secret")
	}
	if redirectURI == "" || state == "" || challenge == "" {
		return "", errors.New("incomplete github web authorization request")
	}
	u, err := url.Parse(c.authorizeURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI, verifier string) (Token, error) {
	if !c.WebEnabled() {
		return Token{}, errors.New("github web authorization needs an app client secret")
	}
	if code == "" || redirectURI == "" || verifier == "" {
		return Token{}, errors.New("incomplete github web authorization response")
	}
	var out tokenResponse
	err := c.form(ctx, c.tokenURL, url.Values{
		"client_id": {c.clientID}, "client_secret": {c.clientSecret}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}, &out)
	if err != nil {
		return Token{}, err
	}
	if out.Error != "" {
		return Token{}, oauthError(out.Error, out.Description)
	}
	return c.token(out)
}

type DeviceCode struct {
	Code            string
	UserCode        string
	VerificationURI string
	Interval        time.Duration
	ExpiresAt       time.Time
}

type Token struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

func (c *Client) Start(ctx context.Context) (DeviceCode, error) {
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	if err := c.form(ctx, c.codeURL, url.Values{"client_id": {c.clientID}}, &out); err != nil {
		return DeviceCode{}, err
	}
	if out.Error != "" {
		return DeviceCode{}, oauthError(out.Error, out.Description)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return DeviceCode{}, errors.New("github returned an incomplete device code")
	}
	interval := time.Duration(out.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	uri := out.VerificationURI
	if uri == "" {
		uri = "https://github.com/login/device"
	}
	return DeviceCode{Code: out.DeviceCode, UserCode: out.UserCode, VerificationURI: uri,
		Interval: interval, ExpiresAt: c.now().Add(ttl)}, nil
}

// Poll asks once. The Manager enforces the cadence and retains the device code,
// so neither the secret code nor a refresh token ever crosses into a guest.
func (c *Client) Poll(ctx context.Context, dc DeviceCode) (Token, error) {
	var out tokenResponse
	err := c.form(ctx, c.tokenURL, url.Values{
		"client_id": {c.clientID}, "device_code": {dc.Code},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &out)
	if err != nil {
		return Token{}, err
	}
	if out.Error != "" {
		return Token{}, oauthError(out.Error, out.Description)
	}
	return c.token(out)
}

type ScopedToken struct {
	AccessToken     string
	AccessExpiresAt time.Time
}

// Scope derives the only token a guest is allowed to receive from the broad
// user grant returned by OAuth. GitHub authenticates this endpoint with the
// App's client credentials and attributes the derived ghu_ token to the same
// user while restricting it to one target, repository, and permission set.
func (c *Client) Scope(ctx context.Context, accessToken, target string, repoID int64, permissions map[string]string) (ScopedToken, error) {
	if !c.WebEnabled() {
		return ScopedToken{}, errors.New("github scoped user tokens need an app client secret")
	}
	if accessToken == "" || strings.TrimSpace(target) == "" || repoID <= 0 || len(permissions) == 0 {
		return ScopedToken{}, errors.New("incomplete github scoped token request")
	}
	body, err := json.Marshal(struct {
		AccessToken   string            `json:"access_token"`
		Target        string            `json:"target"`
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{AccessToken: accessToken, Target: target, RepositoryIDs: []int64{repoID}, Permissions: permissions})
	if err != nil {
		return ScopedToken{}, err
	}
	endpoint := c.apiURL + "/applications/" + url.PathEscape(c.clientID) + "/token/scoped"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ScopedToken{}, err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.hc.Do(req)
	if err != nil {
		return ScopedToken{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ScopedToken{}, fmt.Errorf("github returned %s creating a scoped user token", resp.Status)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return ScopedToken{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return ScopedToken{}, errors.New("github returned an incomplete scoped user token")
	}
	return ScopedToken{AccessToken: out.Token, AccessExpiresAt: out.ExpiresAt}, nil
}

func (c *Client) Refresh(ctx context.Context, refresh string) (Token, error) {
	if refresh == "" {
		return Token{}, ErrBadRefresh
	}
	var out tokenResponse
	values := url.Values{
		"client_id": {c.clientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh},
	}
	// GitHub requires the secret when refreshing a token issued by the web
	// application flow. It is accepted for device-flow refreshes too, so a
	// gateway can refresh grants from either front door without persisting
	// which OAuth ceremony created them.
	if c.clientSecret != "" {
		values.Set("client_secret", c.clientSecret)
	}
	err := c.form(ctx, c.tokenURL, values, &out)
	if err != nil {
		return Token{}, err
	}
	if out.Error != "" {
		return Token{}, oauthError(out.Error, out.Description)
	}
	return c.token(out)
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_token_expires_in"`
	Error            string `json:"error"`
	Description      string `json:"error_description"`
}

func (c *Client) token(out tokenResponse) (Token, error) {
	if out.AccessToken == "" || out.RefreshToken == "" || out.ExpiresIn <= 0 || out.RefreshExpiresIn <= 0 {
		return Token{}, ErrNoRefresh
	}
	now := c.now()
	return Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		AccessExpiresAt:  now.Add(time.Duration(out.ExpiresIn) * time.Second),
		RefreshExpiresAt: now.Add(time.Duration(out.RefreshExpiresIn) * time.Second)}, nil
}

// VerifyUser binds a token to the immutable GitHub account recorded for the
// Sparkbox owner. It is used on the broad OAuth result before it is scoped.
func (c *Client) VerifyUser(ctx context.Context, token string, githubID int64) error {
	var user struct {
		ID int64 `json:"id"`
	}
	if err := c.api(ctx, "/user", token, &user); err != nil {
		return err
	}
	if user.ID != githubID {
		return fmt.Errorf("%w: got account %d, want %d", ErrWrongUser, user.ID, githubID)
	}
	return nil
}

// Verify binds the derived grant to the immutable user and proves the scoped
// token exposes exactly the requested repository. A broad token is refused.
func (c *Client) Verify(ctx context.Context, token string, githubID, installationID, repoID int64) error {
	if err := c.VerifyUser(ctx, token, githubID); err != nil {
		return err
	}
	var listing struct {
		TotalCount   int `json:"total_count"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	path := fmt.Sprintf("/user/installations/%d/repositories?per_page=100", installationID)
	if err := c.api(ctx, path, token, &listing); err != nil {
		return err
	}
	if listing.TotalCount != 1 || len(listing.Repositories) != 1 || listing.Repositories[0].ID != repoID {
		return fmt.Errorf("%w: token exposes %d repositories", ErrWrongScope, listing.TotalCount)
	}
	return nil
}

func (c *Client) api(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github returned %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (c *Client) form(ctx context.Context, endpoint string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github returned %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func oauthError(code, description string) error {
	var base error
	switch code {
	case "authorization_pending":
		base = ErrPending
	case "slow_down":
		base = ErrSlowDown
	case "access_denied":
		base = ErrDenied
	case "expired_token":
		base = ErrExpired
	case "bad_refresh_token":
		base = ErrBadRefresh
	default:
		base = fmt.Errorf("github refused OAuth request: %s", code)
	}
	if description == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, description)
}
