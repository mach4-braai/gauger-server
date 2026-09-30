// Package config reads gauger-server settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL string

	TSDir      string
	TSHostname string
	TSAuthKey  string

	Retention time.Duration
}

func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL: getenv("GAUGER_DATABASE_URL"),
		TSDir:       orDefault(getenv("GAUGER_TS_DIR"), "/var/lib/gauger-server/tsnet"),
		TSHostname:  orDefault(getenv("GAUGER_TS_HOSTNAME"), "gauger-server"),
		TSAuthKey:   getenv("GAUGER_TS_AUTHKEY"),
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("GAUGER_DATABASE_URL is required")
	}
	days, err := positiveInt(getenv, "GAUGER_RETENTION_DAYS", 90)
	if err != nil {
		return Config{}, err
	}
	c.Retention = time.Duration(days) * 24 * time.Hour
	return c, nil
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
