// Package config reads gauger-server settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
)

type Config struct {
	DatabaseURL string

	TSDir      string
	TSHostname string
	TSAuthKey  string

	Retention time.Duration

	GitHubAPIURL string
	GitHubURL    string
	// GitHubApp is set when the App comes from environment variables
	// instead of the manifest flow.
	GitHubApp *github.Credentials

	RunnerTag    string
	OIDCAudience string
	OIDCOwnerID  string
}

func Load() (Config, error) {
	return load(os.Getenv, os.ReadFile)
}

func load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	c := Config{
		DatabaseURL:  getenv("GAUGER_DATABASE_URL"),
		TSDir:        orDefault(getenv("GAUGER_TS_DIR"), "/var/lib/gauger-server/tsnet"),
		TSHostname:   orDefault(getenv("GAUGER_TS_HOSTNAME"), "gauger-server"),
		TSAuthKey:    getenv("GAUGER_TS_AUTHKEY"),
		GitHubAPIURL: strings.TrimSuffix(orDefault(getenv("GAUGER_GITHUB_API_URL"), "https://api.github.com"), "/"),
		GitHubURL:    strings.TrimSuffix(orDefault(getenv("GAUGER_GITHUB_URL"), "https://github.com"), "/"),
		RunnerTag:    orDefault(getenv("GAUGER_RUNNER_TAG"), "tag:gauger-ci"),
		OIDCAudience: orDefault(getenv("GAUGER_OIDC_AUDIENCE"), "gauger-server"),
		OIDCOwnerID:  orDefault(getenv("GAUGER_OIDC_REPOSITORY_OWNER_ID"), "287937105"),
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("GAUGER_DATABASE_URL is required")
	}
	days, err := positiveInt(getenv, "GAUGER_RETENTION_DAYS", 90)
	if err != nil {
		return Config{}, err
	}
	c.Retention = time.Duration(days) * 24 * time.Hour

	app, err := loadApp(getenv, readFile)
	if err != nil {
		return Config{}, err
	}
	c.GitHubApp = app
	return c, nil
}

func loadApp(getenv func(string) string, readFile func(string) ([]byte, error)) (*github.Credentials, error) {
	id := getenv("GAUGER_GITHUB_APP_ID")
	keyFile := getenv("GAUGER_GITHUB_APP_PRIVATE_KEY_FILE")
	secret := getenv("GAUGER_GITHUB_WEBHOOK_SECRET")
	if id == "" && keyFile == "" && secret == "" {
		return nil, nil
	}
	if id == "" || keyFile == "" || secret == "" {
		return nil, fmt.Errorf("set all of GAUGER_GITHUB_APP_ID, GAUGER_GITHUB_APP_PRIVATE_KEY_FILE and GAUGER_GITHUB_WEBHOOK_SECRET, or none")
	}
	appID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || appID <= 0 {
		return nil, fmt.Errorf("GAUGER_GITHUB_APP_ID must be a positive integer, got %q", id)
	}
	pem, err := readFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("GAUGER_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("GAUGER_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
	}
	return &github.Credentials{AppID: appID, PrivateKey: key, WebhookSecret: secret, FromEnv: true}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func positiveInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return n, nil
}
