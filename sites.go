package main

import (
	"crypto/rand"
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var privacyModes = []string{"cookieless", "consent", "cookies"}

func toPrivacy(v string) string {
	if slices.Contains(privacyModes, v) {
		return v
	}
	return "cookieless"
}

type Site struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	Key       string   `json:"key"`
	Privacy   string   `json:"privacy"`
	CreatedAt int64    `json:"created_at"`
}

func toSite(r Row) Site {
	return Site{
		ID: i64(r, "id"), Name: str(r, "name"), Domains: splitDomains(str(r, "domains")),
		Key: str(r, "key"), Privacy: toPrivacy(str(r, "privacy")), CreatedAt: i64(r, "created_at"),
	}
}

var (
	domainSep    = regexp.MustCompile(`[\s,]+`)
	domainScheme = regexp.MustCompile(`^https?://`)
	domainTail   = regexp.MustCompile(`[/:].*$`)
)

// splitDomains normalises "https://www.example.com/, app.example.com" to ["example.com", "app.example.com"].
func splitDomains(value string) []string {
	out := []string{}
	for _, d := range domainSep.Split(value, -1) {
		d = strings.ToLower(strings.TrimSpace(d))
		d = domainScheme.ReplaceAllString(d, "")
		d = domainTail.ReplaceAllString(d, "")
		d = strings.TrimPrefix(d, "www.")
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// Sites are looked up on every collect request, so keep them in memory.
var (
	sitesMu    sync.RWMutex
	sitesByKey = map[string]Site{}
)

func reloadSites() error {
	rows, err := rowsOf(db, "SELECT * FROM sites")
	if err != nil {
		return err
	}
	m := make(map[string]Site, len(rows))
	for _, r := range rows {
		s := toSite(r)
		m[s.Key] = s
	}
	sitesMu.Lock()
	sitesByKey = m
	sitesMu.Unlock()
	return nil
}

func siteByKey(key string) (Site, bool) {
	sitesMu.RLock()
	defer sitesMu.RUnlock()
	s, ok := sitesByKey[key]
	return s, ok
}

func listSites() ([]Site, error) {
	rows, err := rowsOf(db, "SELECT * FROM sites ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	out := make([]Site, len(rows))
	for i, r := range rows {
		out[i] = toSite(r)
	}
	return out, nil
}

func getSite(id int64) (Site, bool) {
	r, err := rowOf(db, "SELECT * FROM sites WHERE id = ?", id)
	if err != nil || r == nil {
		return Site{}, false
	}
	return toSite(r), true
}

func createSite(name, domains, privacy string) (Site, error) {
	b := make([]byte, 9)
	rand.Read(b)
	key := "site_" + base64.RawURLEncoding.EncodeToString(b)
	res, err := db.Exec("INSERT INTO sites (name, domains, key, privacy, created_at) VALUES (?, ?, ?, ?, ?)",
		strings.TrimSpace(name), strings.Join(splitDomains(domains), ","), key, toPrivacy(privacy), time.Now().UnixMilli())
	if err != nil {
		return Site{}, err
	}
	id, _ := res.LastInsertId()
	reloadSites()
	s, _ := getSite(id)
	return s, nil
}

func updateSite(id int64, name, domains, privacy string) (Site, error) {
	if _, err := db.Exec("UPDATE sites SET name = ?, domains = ?, privacy = ? WHERE id = ?",
		strings.TrimSpace(name), strings.Join(splitDomains(domains), ","), toPrivacy(privacy), id); err != nil {
		return Site{}, err
	}
	reloadSites()
	s, _ := getSite(id)
	return s, nil
}

func deleteSite(id int64) error {
	_, err := db.Exec("DELETE FROM sites WHERE id = ?", id)
	reloadSites()
	return err
}

// hostAllowed reports whether a page on hostname may report to the site. Subdomains of a listed domain are allowed.
func hostAllowed(s Site, hostname string) bool {
	if len(s.Domains) == 0 {
		return true
	}
	host := strings.TrimPrefix(strings.ToLower(hostname), "www.")
	for _, d := range s.Domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// siteScope is the sites a dashboard request covers: one site, or every site ("All sites").
type siteScope struct {
	ids []int64
	all bool
}

// where matches col against the scope's sites. Every index leads with site_id, so an IN list still uses them.
func (sc siteScope) where(col string) (string, []any) {
	if len(sc.ids) == 1 {
		return col + " = ?", []any{sc.ids[0]}
	}
	args := make([]any, len(sc.ids))
	for i, id := range sc.ids {
		args[i] = id
	}
	return col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")", args
}

// liveKey is the scope's key in the live presence: the site's id, or allSitesKey.
func (sc siteScope) liveKey() int64 {
	if sc.all {
		return allSitesKey
	}
	return sc.ids[0]
}
