package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Hit is the payload sent by tracker/t.js. Short keys keep beacons small.
type Hit struct {
	K   string          `json:"k"`   // site key
	T   string          `json:"t"`   // pageview | ping | leave | event | identify | consent
	V   string          `json:"v"`   // visitor id from the first-party cookie (cookie modes only)
	S   string          `json:"s"`   // session id from the first-party cookie (cookie modes only)
	P   string          `json:"p"`   // pageview id (lives in memory for one page load)
	PP  string          `json:"pp"`  // previous pageview id in the same page load (SPA navigation)
	U   string          `json:"u"`   // page URL
	R   string          `json:"r"`   // document.referrer
	Ti  string          `json:"ti"`  // document.title
	Sw  float64         `json:"sw"`  // screen width
	Sh  float64         `json:"sh"`  // screen height
	L   string          `json:"l"`   // language
	Tz  string          `json:"tz"`  // IANA timezone
	E   float64         `json:"e"`   // engaged ms on the current page
	En  string          `json:"en"`  // event name
	Ep  json.RawMessage `json:"ep"`  // event props
	UID string          `json:"uid"` // identify: your user id
	Tr  json.RawMessage `json:"tr"`  // identify: traits (email, name, plan...)
	BS  int64           `json:"bs"`  // browser signals for bot detection (bits, see bots.go)
	I   int             `json:"i"`   // 1 once a person has scrolled, clicked, tapped or typed on this page
}

type identity struct {
	visitorID, sessionID, method string // method: "cookie" or "hash" (cookieless)
}

type collectError struct {
	status int
	msg    string
}

var (
	idRE           = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	maxEngagedMs   = int64(4 * time.Hour / time.Millisecond)
	sessionTimeout = 30 * time.Minute
)

// jsonObject keeps a small JSON object (event props, traits) as a string, or returns nil.
func jsonObject(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || len(raw) > 4000 {
		return nil
	}
	return string(raw)
}

func collect(r *http.Request, body []byte) *collectError {
	var hit Hit
	if err := json.Unmarshal(body, &hit); err != nil {
		return &collectError{400, "Invalid JSON"}
	}
	site, ok := siteByKey(hit.K)
	if !ok {
		return &collectError{404, "Unknown site key"}
	}
	for name, v := range map[string]string{"v": hit.V, "s": hit.S, "p": hit.P, "pp": hit.PP} {
		if v != "" && !idRE.MatchString(v) {
			return &collectError{400, "Invalid " + name}
		}
	}
	ua := r.UserAgent()
	if isBot(ua) {
		return nil
	}
	// The Origin header is set by the browser and can't be changed by page scripts, so check it when present.
	if origin := refHost(r.Header.Get("Origin")); origin != "" && !hostAllowed(site, origin) {
		return &collectError{403, "Domain not allowed for this site"}
	}

	now := time.Now()
	switch hit.T {
	case "pageview":
		return pageview(site, &hit, r, ua, now)
	case "ping", "leave":
		return heartbeat(site, &hit, now)
	case "event":
		return event(site, &hit, now)
	case "identify":
		return identify(site, &hit, now)
	case "consent":
		return consent(site, &hit, now)
	}
	return &collectError{400, "Unknown hit type"}
}

// countryOf prefers a CDN's country header, then the GeoIP database. "XX"/"T1" (unknown/Tor) count as unknown.
func countryOf(r *http.Request, ip string) string {
	for _, h := range []string{"CF-IPCountry", "X-Vercel-IP-Country", "X-Country-Code"} {
		if v := strings.ToUpper(strings.TrimSpace(r.Header.Get(h))); len(v) == 2 && v != "XX" && v != "T1" {
			return v
		}
	}
	return lookupCountry(ip)
}

// cookieIdentity uses the cookie ids, but only when the site allows cookies.
func cookieIdentity(site Site, hit *Hit) *identity {
	if site.Privacy == "cookieless" || hit.V == "" || hit.S == "" {
		return nil
	}
	return &identity{hit.V, hit.S, "cookie"}
}

// pageviewIdentity: the cookie if allowed, otherwise a cookieless id derived on the server.
func pageviewIdentity(site Site, hit *Hit, r *http.Request, now time.Time) identity {
	if c := cookieIdentity(site, hit); c != nil {
		return *c
	}
	cutoff := now.Add(-sessionTimeout).UnixMilli()

	// Same page load (single-page-app navigation): stay in that session even if the IP changed.
	if hit.PP != "" {
		prev, _ := rowOf(db, `SELECT s.id, s.visitor_id, s.id_method FROM pageviews p JOIN sessions s ON s.id = p.session_id
			WHERE p.id = ? AND p.site_id = ? AND s.last_seen >= ?`, hit.PP, site.ID, cutoff)
		if prev != nil {
			return identity{str(prev, "visitor_id"), str(prev, "id"), str(prev, "id_method")}
		}
	}

	// Otherwise join this visitor's open session (another tab, or the next page), or start a new one.
	visitorID := cookielessVisitorID(site.ID, r, now)
	open, _ := rowOf(db, `SELECT id FROM sessions WHERE site_id = ? AND visitor_id = ? AND last_seen >= ?
		ORDER BY last_seen DESC LIMIT 1`, site.ID, visitorID, cutoff)
	sessionID := str(open, "id")
	if sessionID == "" {
		sessionID = newID()
	}
	return identity{visitorID, sessionID, "hash"}
}

// existingIdentity finds the session a ping/event/identify belongs to.
func existingIdentity(site Site, hit *Hit) *identity {
	if c := cookieIdentity(site, hit); c != nil {
		if s, _ := rowOf(db, "SELECT site_id FROM sessions WHERE id = ?", c.sessionID); s != nil && i64(s, "site_id") == site.ID {
			return c
		}
	}
	if hit.P == "" {
		return nil
	}
	pv, _ := rowOf(db, "SELECT session_id, visitor_id FROM pageviews WHERE id = ? AND site_id = ?", hit.P, site.ID)
	if pv == nil {
		return nil
	}
	return &identity{str(pv, "visitor_id"), str(pv, "session_id"), "hash"}
}

func pageview(site Site, hit *Hit, r *http.Request, ua string, now time.Time) *collectError {
	u, err := url.Parse(hit.U)
	if err != nil || u.Hostname() == "" {
		return &collectError{400, "Invalid url"}
	}
	if hit.P == "" {
		return &collectError{400, "Missing pageview id"}
	}
	if !hostAllowed(site, u.Hostname()) {
		return &collectError{403, "Domain not allowed for this site"}
	}

	id := pageviewIdentity(site, hit, r, now)
	owner, _ := rowOf(db, "SELECT site_id FROM sessions WHERE id = ?", id.sessionID)
	if owner != nil && i64(owner, "site_id") != site.ID {
		return &collectError{409, "Session belongs to another site"}
	}

	path := truncate(u.EscapedPath(), 500)
	if path == "" {
		path = "/"
	}
	title := truncate(hit.Ti, 300)
	// A referrer from our own host is internal navigation, not a traffic source.
	referrer := ""
	if h := refHost(hit.R); h != "" && h != strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
		referrer = truncate(hit.R, 1000)
	}
	q := u.Query()
	utmSource := q.Get("utm_source")
	if utmSource == "" {
		utmSource = q.Get("ref")
	}
	utmSource = truncate(utmSource, 100)
	ag := parseUA(ua, hit.Sw)
	ip := clientIP(r)
	network := truncate(lookupNetwork(ip), 200)
	signals := requestSignals(r, hit.BS, ag, network, owner == nil, ip)
	country := countryOf(r, ip)
	screen := ""
	if hit.Sw > 0 && hit.Sh > 0 {
		screen = strings.Join([]string{itoa(int64(hit.Sw)), itoa(int64(hit.Sh))}, "x")
	}

	ms := now.UnixMilli()
	lv := LiveVisitor{
		SessionID: id.sessionID, VisitorID: id.visitorID, Path: path, Title: strPtr(title),
		Source: classifySource(referrer, utmSource), Device: ag.Device, Browser: ag.Browser, Country: strPtr(country),
		StartedAt: ms, PageStartedAt: ms, LastSeen: ms, Pageviews: 1,
	}

	err = withTx(func(tx *sql.Tx) error {
		cookieless := 0
		if id.method == "hash" {
			cookieless = 1
		}
		if _, err := tx.Exec(`INSERT INTO visitors (site_id, id, first_seen, last_seen, cookieless) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (site_id, id) DO UPDATE SET last_seen = excluded.last_seen`, site.ID, id.visitorID, ms, ms, cookieless); err != nil {
			return err
		}
		visitor, err := rowOf(tx, "SELECT user_id, traits FROM visitors WHERE site_id = ? AND id = ?", site.ID, id.visitorID)
		if err != nil {
			return err
		}
		lv.UserID, lv.UserName = strPtr(str(visitor, "user_id")), nameFromTraits(str(visitor, "traits"))

		existing, err := rowOf(tx, "SELECT pageviews, source, started_at FROM sessions WHERE id = ?", id.sessionID)
		if err != nil {
			return err
		}
		if existing != nil {
			if _, err := tx.Exec(`UPDATE sessions SET last_seen = ?, exit_path = ?, exit_title = ?, pageviews = pageviews + 1,
				bot_signals = bot_signals | ? WHERE id = ?`,
				ms, path, nullable(title), signals, id.sessionID); err != nil {
				return err
			}
			lv.Pageviews, lv.Source, lv.StartedAt = i64(existing, "pageviews")+1, str(existing, "source"), i64(existing, "started_at")
		} else if _, err := tx.Exec(`INSERT INTO sessions (id, site_id, visitor_id, id_method, started_at, last_seen, entry_path, exit_path,
				exit_title, pageviews, referrer, source, utm_source, utm_medium, utm_campaign, browser, os, device, country, language, timezone, screen,
			network, bot_signals)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id.sessionID, site.ID, id.visitorID, id.method, ms, ms, path, path, nullable(title),
			nullable(referrer), lv.Source, nullable(utmSource), nullable(truncate(q.Get("utm_medium"), 100)),
			nullable(truncate(q.Get("utm_campaign"), 200)), ag.Browser, ag.OS, ag.Device, nullable(country),
			nullable(truncate(hit.L, 20)), nullable(truncate(hit.Tz, 64)), nullable(screen),
			nullable(network), signals|sigNoInteraction.bit); err != nil {
			return err
		}
		if lv.Bot, err = rescore(tx, id.sessionID, ms); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT OR IGNORE INTO pageviews (id, site_id, session_id, visitor_id, ts, path, title) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			hit.P, site.ID, id.sessionID, id.visitorID, ms, path, nullable(title))
		return err
	})
	if err != nil {
		return &collectError{500, err.Error()}
	}
	live.touch(site.ID, lv)
	return nil
}

func heartbeat(site Site, hit *Hit, now time.Time) *collectError {
	id := existingIdentity(site, hit)
	if id == nil {
		return nil
	}
	engaged := min(max(int64(hit.E), 0), maxEngagedMs)
	ms := now.UnixMilli()
	if hit.P != "" {
		db.Exec("UPDATE pageviews SET duration = max(duration, ?) WHERE id = ? AND site_id = ?", engaged, hit.P, site.ID)
	}
	db.Exec("UPDATE sessions SET last_seen = ? WHERE id = ? AND site_id = ?", ms, id.sessionID, site.ID)
	noteBrowser(site.ID, id.sessionID, hit, ms)
	if hit.T == "leave" {
		live.leave(site.ID, id.sessionID)
	} else if !live.ping(site.ID, id.sessionID, ms) {
		// Server restarted or the visitor came back to a background tab: put them back on the live list.
		s, _ := rowOf(db, `SELECT s.*, v.user_id, v.traits FROM sessions s
			LEFT JOIN visitors v ON v.site_id = s.site_id AND v.id = s.visitor_id WHERE s.id = ?`, id.sessionID)
		if s != nil {
			live.touch(site.ID, liveFromSession(s, ms-engaged))
		}
	}
	return nil
}

func event(site Site, hit *Hit, now time.Time) *collectError {
	name := truncate(hit.En, 100)
	if name == "" {
		return &collectError{400, "Missing event name"}
	}
	id := existingIdentity(site, hit)
	if id == nil {
		return &collectError{409, "Send a pageview before events"}
	}
	var path any
	if u, err := url.Parse(hit.U); err == nil && hit.U != "" {
		path = truncate(u.EscapedPath(), 500)
	}
	ms := now.UnixMilli()
	if _, err := db.Exec("INSERT INTO events (site_id, session_id, visitor_id, ts, name, path, props) VALUES (?, ?, ?, ?, ?, ?, ?)",
		site.ID, id.sessionID, id.visitorID, ms, name, path, jsonObject(hit.Ep)); err != nil {
		return &collectError{500, err.Error()}
	}
	db.Exec("UPDATE sessions SET last_seen = ? WHERE id = ?", ms, id.sessionID)
	noteBrowser(site.ID, id.sessionID, hit, ms)
	return nil
}

// noteBrowser records a first interaction and browser checks that finished after the page view, then rescores.
func noteBrowser(siteID int64, sessionID string, hit *Hit, ms int64) {
	if hit.I != 1 && hit.BS == 0 {
		return
	}
	s, _ := rowOf(db, "SELECT browser, device, interacted, bot_signals FROM sessions WHERE id = ?", sessionID)
	if s == nil {
		return
	}
	signals := i64(s, "bot_signals") | clientSignals(hit.BS, str(s, "browser"), str(s, "device"))
	interacted := i64(s, "interacted")
	if hit.I == 1 {
		interacted = 1
		signals &^= sigNoInteraction.bit
	}
	if signals == i64(s, "bot_signals") && interacted == i64(s, "interacted") {
		return
	}
	db.Exec("UPDATE sessions SET bot_signals = ?, interacted = ? WHERE id = ?", signals, interacted, sessionID)
	if bot, err := rescore(db, sessionID, ms); err == nil {
		live.setBot(siteID, sessionID, bot)
	}
}

// rescore recomputes a session's bot score from its stored signals and pace.
func rescore(q querier, sessionID string, now int64) (bool, error) {
	s, err := rowOf(q, "SELECT bot_signals, interacted, pageviews, started_at FROM sessions WHERE id = ?", sessionID)
	if err != nil || s == nil {
		return false, err
	}
	signals := i64(s, "bot_signals")
	if fastNavigation(i64(s, "pageviews"), i64(s, "started_at"), now) {
		signals |= sigFastNavigation.bit
	}
	score := botScore(signals, i64(s, "interacted") == 1)
	bot := score >= botThreshold
	_, err = q.Exec("UPDATE sessions SET bot_signals = ?, bot_score = ?, bot = ? WHERE id = ?", signals, score, bot, sessionID)
	return bot, err
}

func identify(site Site, hit *Hit, now time.Time) *collectError {
	userID := truncate(hit.UID, 200)
	if userID == "" {
		return &collectError{400, "Missing user id"}
	}
	id := existingIdentity(site, hit)
	if id == nil {
		return &collectError{409, "Send a pageview before identify"}
	}
	traits := jsonObject(hit.Tr)
	if _, err := db.Exec("UPDATE visitors SET user_id = ?, traits = coalesce(?, traits), last_seen = ? WHERE site_id = ? AND id = ?",
		userID, traits, now.UnixMilli(), site.ID, id.visitorID); err != nil {
		return &collectError{500, err.Error()}
	}
	t, _ := traits.(string)
	live.identify(site.ID, id.sessionID, userID, nameFromTraits(t))
	return nil
}

// consent: the visitor accepted cookies part-way through a cookieless session. Move that session over to the
// new cookie ids so the visit isn't counted twice.
func consent(site Site, hit *Hit, now time.Time) *collectError {
	if site.Privacy == "cookieless" {
		return nil
	}
	if hit.V == "" || hit.S == "" || hit.P == "" {
		return &collectError{400, "Missing ids"}
	}
	pv, _ := rowOf(db, "SELECT session_id, visitor_id FROM pageviews WHERE id = ? AND site_id = ?", hit.P, site.ID)
	if pv == nil || str(pv, "session_id") == hit.S {
		return nil
	}
	if taken, _ := rowOf(db, "SELECT 1 FROM sessions WHERE id = ?", hit.S); taken != nil {
		return nil
	}
	oldSession, oldVisitor := str(pv, "session_id"), str(pv, "visitor_id")
	ms := now.UnixMilli()

	err := withTx(func(tx *sql.Tx) error {
		before, err := rowOf(tx, "SELECT first_seen, user_id, traits FROM visitors WHERE site_id = ? AND id = ?", site.ID, oldVisitor)
		if err != nil {
			return err
		}
		firstSeen := ms
		if before != nil {
			firstSeen = i64(before, "first_seen")
		}
		steps := []struct {
			q    string
			args []any
		}{
			{`INSERT INTO visitors (site_id, id, first_seen, last_seen, user_id, traits) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (site_id, id) DO UPDATE SET last_seen = excluded.last_seen,
				user_id = coalesce(visitors.user_id, excluded.user_id), traits = coalesce(visitors.traits, excluded.traits)`,
				[]any{site.ID, hit.V, firstSeen, ms, nullable(str(before, "user_id")), nullable(str(before, "traits"))}},
			{"UPDATE sessions SET id = ?, visitor_id = ?, id_method = 'cookie' WHERE id = ?", []any{hit.S, hit.V, oldSession}},
			{"UPDATE pageviews SET session_id = ?, visitor_id = ? WHERE session_id = ?", []any{hit.S, hit.V, oldSession}},
			{"UPDATE events SET session_id = ?, visitor_id = ? WHERE session_id = ?", []any{hit.S, hit.V, oldSession}},
		}
		for _, st := range steps {
			if _, err := tx.Exec(st.q, st.args...); err != nil {
				return err
			}
		}
		if oldVisitor != hit.V {
			_, err = tx.Exec(`DELETE FROM visitors WHERE site_id = ? AND id = ?
				AND NOT EXISTS (SELECT 1 FROM sessions WHERE site_id = ? AND visitor_id = ?)`, site.ID, oldVisitor, site.ID, oldVisitor)
		}
		return err
	})
	if err != nil {
		return &collectError{500, err.Error()}
	}
	live.rekey(site.ID, oldSession, hit.S, hit.V)
	return nil
}
