package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	saltMu  sync.Mutex
	saltDay string
	saltVal string
)

// currentSalt returns today's salt (UTC). Creating a new day's salt deletes every older one.
func currentSalt(now time.Time) string {
	day := now.UTC().Format("2006-01-02")
	saltMu.Lock()
	defer saltMu.Unlock()
	if saltDay == day {
		return saltVal
	}
	b := make([]byte, 32)
	rand.Read(b)
	db.Exec("INSERT OR IGNORE INTO salts (day, salt) VALUES (?, ?)", day, base64.StdEncoding.EncodeToString(b))
	r, _ := rowOf(db, "SELECT salt FROM salts WHERE day = ?", day)
	db.Exec("DELETE FROM salts WHERE day <> ?", day)
	saltDay, saltVal = day, str(r, "salt")
	return saltVal
}

// clientIP is the visitor's IP. Proxy headers are honoured so this works behind Cloudflare, nginx, Caddy, etc.
func clientIP(r *http.Request) string {
	for _, h := range []string{"CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For"} {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(h), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// cookielessVisitorID hashes today's secret salt, the site, the IP and the user agent. Nothing is stored on the
// device, and once the salt is deleted at the end of the day the id can't be linked to other days or reversed.
func cookielessVisitorID(siteID int64, r *http.Request, now time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s|%s", currentSalt(now), siteID, clientIP(r), r.UserAgent()))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
