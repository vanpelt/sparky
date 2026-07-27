package users

// GitHub's OAuth device flow: the linking path for people whose SSH key is not
// published on GitHub.
//
// The key check next door (VerifyGitHubKey) proves control of a GitHub account
// by finding one of this account's registered keys among the ones github.com
// publishes for a login. It is excellent — no OAuth app, no client secret, no
// browser — and it only works for somebody who publishes a key there and uses
// that same key here. Plenty of people do neither: they push over HTTPS, or
// they keep a separate key for sandboxes. For them the platform had nothing.
//
// The device flow is the standard answer (RFC 8628) and fits an SSH session
// better than a redirect ever could: there is no callback URL, nothing to
// paste back, and no secret on this side. We print a short code, the person
// types it into github.com in whatever browser they already have open, and we
// poll until GitHub tells us who authorized it. The client id is a public
// identifier — it appears in the request that mints the code and is safe in a
// flag, a config file and a log line.
//
// What this deliberately does NOT do is ask for any repository access. No scope
// is requested at all, so an OAuth app's token can read public profile data and
// nothing else, and a GitHub App's token carries only the permissions the app
// was installed with. We are asking GitHub one question — "who is this?" — and
// a token that could do more would be a token worth stealing.

import (
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

// The device flow's three endpoints. Fields on the client rather than package
// vars so a test can point one instance at an httptest server without reaching
// into shared state that another parallel test is reading.
const (
	deviceCodeURL  = "https://github.com/login/device/code"
	deviceTokenURL = "https://github.com/login/oauth/access_token"
	githubUserURL  = "https://api.github.com/user"
)

var (
	// ErrDeviceDenied is the person saying no on github.com, or closing the tab
	// on the authorization screen. It is a decision, not a fault, and callers
	// say so rather than offering a retry.
	ErrDeviceDenied = errors.New("the GitHub authorization was declined")
	// ErrDeviceExpired is the code timing out unused. GitHub's own window is
	// 15 minutes; a caller's patience is usually shorter.
	ErrDeviceExpired = errors.New("the GitHub code expired before it was entered")
	// ErrDeviceUnsupported is the configuration fault worth telling an operator
	// about by name: a client id that exists but whose app has device flow
	// switched off. Every user hits it, on the first try, forever, and the raw
	// GitHub message ("device_flow_disabled") names a checkbox rather than the
	// setting it belongs to.
	ErrDeviceUnsupported = errors.New("this GitHub app does not have the device flow enabled")
)

// GitHubDevice runs the device flow for one client id.
type GitHubDevice struct {
	clientID  string
	http      *http.Client
	codeURL   string
	tokenURL  string
	userURL   string
	pollFloor time.Duration
	// slowDown is what a slow_down answer adds to the interval. GitHub
	// documents five seconds; it is a field only so a test can prove the
	// interval grows without spending five real ones proving it.
	slowDown time.Duration
}

// NewGitHubDevice builds a client for clientID. It is a public identifier of an
// OAuth app or a GitHub App — not a secret — and the device flow needs nothing
// else from this side, which is the whole reason it is the flow we run.
func NewGitHubDevice(clientID string) *GitHubDevice {
	return &GitHubDevice{
		clientID: clientID,
		// A client of its own rather than http.DefaultClient: the poll below
		// holds a request open for as long as GitHub takes, and it must not be
		// able to inherit or impose a timeout on anything else in the process.
		http:      &http.Client{Timeout: 30 * time.Second},
		codeURL:   deviceCodeURL,
		tokenURL:  deviceTokenURL,
		userURL:   githubUserURL,
		pollFloor: time.Second,
		slowDown:  5 * time.Second,
	}
}

// DeviceCode is a started flow: what to show a person, and the secret half that
// identifies it back to GitHub.
type DeviceCode struct {
	// UserCode is the short string the person types into github.com, and
	// VerificationURI is where they type it. Both are meant to be read aloud
	// and retyped, which is why GitHub hyphenates the code — print it as sent.
	UserCode        string
	VerificationURI string
	// Code is the flow's secret. Anyone holding it can collect the token this
	// flow mints, so it never reaches a log line, a terminal, or an error.
	Code string
	// Interval is how often GitHub permits a poll, and ExpiresAt is when the
	// user code stops working.
	Interval  time.Duration
	ExpiresAt time.Time
}

// GitHubProfile is who GitHub says somebody is. ID is the immutable account
// number: a login can be renamed, released and re-registered by a stranger, so
// anything that must still be true next year keys on the number.
type GitHubProfile struct {
	Login string
	ID    int64
	Email string
}

// Start asks GitHub for a code pair. The caller shows the user code and the
// URI, then hands the result to Wait.
func (d *GitHubDevice) Start(ctx context.Context) (DeviceCode, error) {
	form := url.Values{"client_id": {d.clientID}}
	var body struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	if err := d.post(ctx, d.codeURL, form, &body); err != nil {
		return DeviceCode{}, err
	}
	if body.Error != "" {
		return DeviceCode{}, deviceError(body.Error, body.Description)
	}
	if body.DeviceCode == "" || body.UserCode == "" {
		return DeviceCode{}, errors.New("github started a device flow with no code in it")
	}
	uri := body.VerificationURI
	if uri == "" {
		uri = "https://github.com/login/device"
	}
	// Both cadences are floored rather than trusted: an interval of zero would
	// turn the poll into a spin against github.com, and an expiry of zero would
	// make Wait give up before its first request.
	interval := time.Duration(body.Interval) * time.Second
	if interval < d.pollFloor {
		interval = d.pollFloor
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return DeviceCode{
		UserCode:        body.UserCode,
		VerificationURI: uri,
		Code:            body.DeviceCode,
		Interval:        interval,
		ExpiresAt:       time.Now().Add(ttl),
	}, nil
}

// Wait polls until the person authorizes, declines, or the code expires, then
// asks GitHub who they are.
//
// The two clocks are both honoured and they are not the same: ctx is the
// caller's patience — an SSH session that will not sit there for a quarter of
// an hour — and dc.ExpiresAt is GitHub's, after which polling can only ever
// return the same refusal. Whichever runs out first ends the wait, and a
// caller's cancellation is reported as its own context error so a timed-out
// dialog does not read as GitHub having said no.
func (d *GitHubDevice) Wait(ctx context.Context, dc DeviceCode) (GitHubProfile, error) {
	interval := dc.Interval
	if interval < d.pollFloor {
		interval = d.pollFloor
	}
	form := url.Values{
		"client_id":   {d.clientID},
		"device_code": {dc.Code},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return GitHubProfile{}, ctx.Err()
		case <-timer.C:
		}
		if !dc.ExpiresAt.IsZero() && time.Now().After(dc.ExpiresAt) {
			return GitHubProfile{}, ErrDeviceExpired
		}
		var body struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if err := d.post(ctx, d.tokenURL, form, &body); err != nil {
			return GitHubProfile{}, err
		}
		switch {
		case body.AccessToken != "":
			return d.identify(ctx, body.AccessToken)
		case body.Error == "authorization_pending":
			// The ordinary case: nobody has typed the code yet.
		case body.Error == "slow_down":
			// GitHub's documented back-off is five seconds ON TOP of the
			// interval, and it is not advisory — keeping the old cadence earns
			// the same answer indefinitely.
			interval += d.slowDown
		case body.Error != "":
			return GitHubProfile{}, deviceError(body.Error, body.Description)
		default:
			return GitHubProfile{}, errors.New("github answered the device poll with neither a token nor an error")
		}
		timer.Reset(interval)
	}
}

// identify spends the token on the one question this flow exists to ask. The
// token is not stored, not returned and not logged: it is used once, here, and
// left to expire.
func (d *GitHubDevice) identify(ctx context.Context, token string) (GitHubProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.userURL, nil)
	if err != nil {
		return GitHubProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.http.Do(req)
	if err != nil {
		return GitHubProfile{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return GitHubProfile{}, fmt.Errorf("github returned %s when asked who authorized the code", resp.Status)
	}
	var profile struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile); err != nil {
		return GitHubProfile{}, err
	}
	// A login GitHub itself just handed us still goes through the same shape
	// check as one a person typed. It becomes a URL we fetch and a string every
	// console prints, and "GitHub would never" is not a property this process
	// can check.
	if !githubLoginOK(profile.Login) {
		return GitHubProfile{}, fmt.Errorf("github named an account this platform cannot record (%q)", profile.Login)
	}
	out := GitHubProfile{Login: profile.Login, ID: profile.ID}
	if email := strings.TrimSpace(profile.Email); ValidEmail(email) {
		out.Email = email
	}
	return out, nil
}

// post sends a form and decodes a JSON answer. Accept is what makes GitHub
// answer these two endpoints in JSON at all — without it they reply with a
// urlencoded body, which decodes as an empty struct and would read here as
// "GitHub said nothing".
func (d *GitHubDevice) post(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github returned %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// deviceError maps GitHub's error codes to the three a caller can act on
// differently, and passes everything else through with GitHub's own wording.
//
// The default case keeps the description rather than replacing it: the codes
// left over are all configuration faults (an unknown client id, a mistyped
// device code, an app whose credentials changed), and the operator reading the
// log wants GitHub's sentence, not ours.
func deviceError(code, description string) error {
	switch code {
	case "access_denied":
		return ErrDeviceDenied
	case "expired_token":
		return ErrDeviceExpired
	case "device_flow_disabled", "unsupported_grant_type":
		return ErrDeviceUnsupported
	}
	if description == "" {
		description = code
	}
	return fmt.Errorf("github refused the device flow: %s", description)
}
