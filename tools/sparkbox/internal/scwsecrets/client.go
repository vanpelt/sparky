// Package scwsecrets is a minimal client for Scaleway Secret Manager, scoped to
// the one thing a sparkbox host does at boot: read fleet secrets by path with no
// UUIDs baked into cloud-init. It deliberately wraps only AccessSecretVersionByPath
// rather than pulling in the full Scaleway SDK — the host has exactly one job here
// and the whole surface is one authenticated GET.
package scwsecrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Scaleway's public API host. Overridable for tests.
const DefaultBaseURL = "https://api.scaleway.com"

// ErrNotFound is returned when the secret (or an enabled version of it) does not
// exist, so callers can treat an optional secret as absent rather than fatal.
var ErrNotFound = errors.New("secret not found")

// ErrDenied is returned on 401/403 — a wrong key or, most likely, the IAM
// policy's request.ip condition rejecting this host. It is not retried: the
// answer won't change on a second call, and a fleet host shouldn't stall its
// boot 12 seconds discovering that.
var ErrDenied = errors.New("access denied")

// Client reads secret payloads by path. The zero value is not usable; use New.
type Client struct {
	baseURL   string
	region    string
	projectID string
	token     string // Scaleway secret key, sent as X-Auth-Token
	http      *http.Client
}

// New builds a client. token is the Scaleway secret key (never logged, never an
// argv flag on the host — it comes from the environment). baseURL may be "" for
// the real API.
func New(baseURL, region, projectID, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		region:    region,
		projectID: projectID,
		token:     token,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// accessResponse mirrors the AccessSecretVersion response. Data is base64 over
// the wire; we decode it ourselves rather than relying on the SDK's []byte
// auto-decode, both because we parse the JSON directly and because it makes the
// wire contract explicit at the one place it matters.
type accessResponse struct {
	Data string `json:"data"`
	Type string `json:"type"`
}

// AccessByPath returns the decoded payload of the latest enabled version of the
// secret named name under folder path (e.g. "/sparkbox/fleet", "console-password"),
// along with its secret type. Returns ErrNotFound if there is no enabled version.
//
// The returned bytes are the raw payload. For a typed secret (e.g. ssh_key) that
// payload is itself a JSON document; unwrapping it is the caller's concern.
func (c *Client) AccessByPath(ctx context.Context, path, name string) (data []byte, secretType string, err error) {
	endpoint := fmt.Sprintf("%s/secret-manager/v1beta1/regions/%s/secrets-by-path/versions/latest_enabled/access",
		c.baseURL, url.PathEscape(c.region))
	q := url.Values{}
	q.Set("project_id", c.projectID)
	q.Set("secret_path", path)
	q.Set("secret_name", name)

	// A boot-time network call on a fresh host races cloud-init's network
	// bring-up and Scaleway's own eventual consistency on a just-created key, so
	// retry a few times before giving up.
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		data, secretType, err = c.accessOnce(ctx, endpoint, q)
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrDenied) {
			return data, secretType, err
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("access %s/%s after retries: %w", path, name, lastErr)
}

func (c *Client) accessOnce(ctx context.Context, endpoint string, q url.Values) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Auth-Token", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var ar accessResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return nil, "", fmt.Errorf("decode access response: %w", err)
		}
		raw, err := base64.StdEncoding.DecodeString(ar.Data)
		if err != nil {
			return nil, "", fmt.Errorf("decode secret payload: %w", err)
		}
		return raw, ar.Type, nil
	case http.StatusNotFound:
		return nil, "", ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		// The most likely real-world failure: the IAM policy's request.ip
		// condition rejected this host's source address, or the key lacks
		// SecretManagerSecretAccess. Surface the body — Scaleway names the
		// reason — so a boot failure is diagnosable without guesswork.
		return nil, "", fmt.Errorf("%w (%s): %s", ErrDenied, resp.Status, strings.TrimSpace(string(body)))
	default:
		return nil, "", fmt.Errorf("scaleway returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

// SSHKeyPayload is the JSON shape of a type=ssh_key secret.
type SSHKeyPayload struct {
	SSHPrivateKey string `json:"ssh_private_key"`
}

// UnwrapSSHKey extracts the PEM from a type=ssh_key payload. Secret Manager
// validates this shape on write, so a stored ssh_key is always this JSON — but
// we check rather than assume, so a mis-typed secret fails loudly here instead
// of writing a JSON blob where a PEM belongs.
func UnwrapSSHKey(payload []byte) ([]byte, error) {
	var k SSHKeyPayload
	if err := json.Unmarshal(payload, &k); err != nil {
		return nil, fmt.Errorf("ssh_key secret is not the expected {\"ssh_private_key\":...} JSON: %w", err)
	}
	if k.SSHPrivateKey == "" {
		return nil, errors.New("ssh_key secret has an empty ssh_private_key")
	}
	return []byte(k.SSHPrivateKey), nil
}
