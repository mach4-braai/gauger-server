package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Manifest is the GitHub App manifest a self-hoster posts to GitHub.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// NewManifest describes an App with the permissions and events gauger-server
// needs, for a server reachable at https://dnsName.
func NewManifest(name, dnsName string) Manifest {
	return Manifest{
		Name:           name,
		URL:            "https://" + dnsName + "/",
		HookAttributes: HookAttributes{URL: "https://" + dnsName + ":8443/webhooks/github", Active: true},
		RedirectURL:    "https://" + dnsName + "/setup/callback",
		DefaultPermissions: map[string]string{
			"actions":  "read",
			"metadata": "read",
		},
		DefaultEvents: []string{"workflow_run", "workflow_job"},
	}
}

// ManifestFormURL is where the browser posts the manifest. An empty org
// creates the App on the signed-in user's account.
func ManifestFormURL(githubURL, org, state string) string {
	path := "/settings/apps/new"
	if org != "" {
		path = "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return githubURL + path + "?state=" + url.QueryEscape(state)
}

// AppConversion is GitHub's reply to a manifest code exchange.
type AppConversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// ConvertManifest exchanges the temporary code from the manifest redirect
// for the new App's credentials. The call needs no authentication.
func (c *Client) ConvertManifest(ctx context.Context, code string) (*AppConversion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIURL+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gauger-server")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, &StatusError{Code: resp.StatusCode, Body: truncate(data)}
	}
	var out AppConversion
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out.ID == 0 || out.PEM == "" || out.WebhookSecret == "" {
		return nil, fmt.Errorf("manifest conversion is missing the App ID, key or webhook secret")
	}
	return &out, nil
}
