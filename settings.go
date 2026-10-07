package main

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// The public address of this Omega server, e.g. https://analytics.example.com. The dashboard uses it to show
// the tracking snippet a site owner should paste. OMEGA_PUBLIC_URL overrides the saved setting.

func getSetting(key string) string {
	r, _ := rowOf(db, "SELECT value FROM settings WHERE key = ?", key)
	return str(r, "value")
}

func setSetting(key, value string) error {
	if value == "" {
		_, err := db.Exec("DELETE FROM settings WHERE key = ?", key)
		return err
	}
	_, err := db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

func publicURL() (value string, fromEnv bool) {
	if v := os.Getenv("OMEGA_PUBLIC_URL"); v != "" {
		if n, err := normalizePublicURL(v); err == nil {
			return n, true
		}
	}
	return getSetting("public_url"), false
}

// normalizePublicURL accepts "analytics.example.com" or "https://analytics.example.com/" and returns
// "https://analytics.example.com". A path is kept (for hosting under a sub-path); query and fragment are not allowed.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("Enter an address like https://analytics.example.com")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("Leave out anything after the host name and path (no ?, # or user name)")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/"), nil
}

type ipDatabaseStatus struct {
	Ready bool   `json:"ready"`
	Built string `json:"built,omitempty"` // YYYY-MM-DD
}

func (d *ipDatabase) status() ipDatabaseStatus {
	if !d.ready() {
		return ipDatabaseStatus{}
	}
	return ipDatabaseStatus{Ready: true, Built: d.built().Format("2006-01-02")}
}

func settingsResponse() map[string]any {
	u, fromEnv := publicURL()
	days, daysFromEnv := replayRetentionDays()
	return map[string]any{
		"publicUrl":        u,
		"publicUrlFromEnv": fromEnv,
		"countryLookup":    countryDB.status(),
		"networkLookup":    networkDB.status(),
		"autoUpdate":       !strings.EqualFold(os.Getenv("OMEGA_IP_DB_UPDATE"), "off"),
		"replay": map[string]any{
			"retentionDays": days, "retentionFromEnv": daysFromEnv, "bytes": replayBytes(), "maxBytes": replayMaxBytes(),
		},
	}
}
