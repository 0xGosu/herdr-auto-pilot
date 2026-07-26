package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Repo is the GitHub project hap releases from. It also names the argument
// `hap update` hands to `herdr plugin install`.
const Repo = "0xGosu/herdr-auto-pilot"

// latestReleaseURL is the only remote endpoint the plugin ever contacts.
// GitHub answers it unauthenticated (rate-limited per IP), which is fine for a
// check that runs at most once every TTL.
const latestReleaseURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// requestTimeout bounds the whole exchange. The check is a nicety, so it fails
// fast rather than holding a goroutine open on a black-holed network.
const requestTimeout = 5 * time.Second

// maxBody caps how much of the response is read: the release payload is a few
// KB, and a cap keeps a misbehaving endpoint from feeding the process memory.
const maxBody = 1 << 20

// DefaultClient is the client Latest uses when passed nil.
func DefaultClient() *http.Client { return &http.Client{Timeout: requestTimeout} }

// Latest reports the newest published release tag ("v0.5.2"). Every failure —
// offline, rate-limited, malformed body — comes back as an error for the
// caller to record and otherwise ignore; nothing here is fatal.
func Latest(ctx context.Context, client *http.Client) (string, error) {
	if client == nil {
		client = DefaultClient()
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hap-update-check")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release check: unexpected status %s", resp.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&payload); err != nil {
		return "", fmt.Errorf("release check: %w", err)
	}
	if !IsRelease(payload.TagName) {
		return "", fmt.Errorf("release check: unusable tag %q", payload.TagName)
	}
	return payload.TagName, nil
}
