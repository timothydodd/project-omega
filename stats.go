package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	hourMs = int64(time.Hour / time.Millisecond)
	dayMs  = 24 * hourMs
)

var ranges = []string{"today", "24h", "3d", "7d", "30d", "90d"}

type statsQuery struct {
	Sites    siteScope
	Range    string
	TzOffset int64  // minutes east of UTC (e.g. -300 for US Eastern winter)
	Path     string // only sessions that viewed this page
	Source   string // only sessions from this source
	Country  string // only sessions from this country (ISO code, or "Unknown")
	Bots     bool   // include sessions that look like bots
}

type window struct{ since, until, bucket int64 }

func startOfLocalDay(ts, off int64) int64 { return floorDiv(ts+off, dayMs)*dayMs - off }

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func windowFor(q statsQuery, now int64) window {
	off := q.TzOffset * 60_000
	switch q.Range {
	case "today":
		return window{startOfLocalDay(now, off), now, hourMs}
	case "24h":
		return window{now - dayMs, now, hourMs}
	case "3d":
		return window{now - 3*dayMs, now, hourMs}
	case "30d":
		return window{startOfLocalDay(now-29*dayMs, off), now, dayMs}
	case "90d":
		return window{startOfLocalDay(now-89*dayMs, off), now, dayMs}
	}
	return window{startOfLocalDay(now-6*dayMs, off), now, dayMs} // 7d
}

// filters adds the page/source filters (sessions alias `s`).
func (q statsQuery) filters(sql string, args []any) (string, []any) {
	if !q.Bots {
		sql += " AND s.bot = 0"
	}
	if q.Source != "" {
		sql += " AND s.source = ?"
		args = append(args, q.Source)
	}
	if q.Country == "Unknown" {
		sql += " AND s.country IS NULL"
	} else if q.Country != "" {
		sql += " AND s.country = ?"
		args = append(args, q.Country)
	}
	if q.Path != "" {
		sql += " AND EXISTS (SELECT 1 FROM pageviews f WHERE f.session_id = s.id AND f.path = ?)"
		args = append(args, q.Path)
	}
	return sql, args
}

// sessionScope matches sessions (alias `s`) active in [since, until).
func (q statsQuery) sessionScope(since, until int64) (string, []any) {
	w, args := q.Sites.where("s.site_id")
	return q.filters(w+" AND s.last_seen >= ? AND s.started_at < ?", append(args, since, until))
}

// pageviewScope matches pageviews (alias `p`, joined to sessions `s`) in [since, until).
func (q statsQuery) pageviewScope(since, until int64) (string, []any) {
	w, args := q.Sites.where("p.site_id")
	return q.filters(w+" AND p.ts >= ? AND p.ts < ?", append(args, since, until))
}

func (q statsQuery) uniques(since, until int64) int64 {
	w, args := q.sessionScope(since, until)
	r, _ := rowOf(db, "SELECT COUNT(DISTINCT s.visitor_id) n FROM sessions s WHERE "+w, args...)
	return i64(r, "n")
}

// isEstimate: cookieless ids reset every day, so over more than a day the same person can be counted once per
// day. Multi-day unique counts are estimates whenever cookieless sessions are included.
func (q statsQuery) isEstimate(since, until int64) bool {
	if until-since <= 25*hourMs {
		return false
	}
	w, args := q.sessionScope(since, until)
	r, _ := rowOf(db, "SELECT 1 x FROM sessions s WHERE "+w+" AND s.id_method = 'hash' LIMIT 1", args...)
	return r != nil
}

type kpis struct {
	Visitors          int64   `json:"visitors"`
	VisitorsEstimated bool    `json:"visitorsEstimated"`
	Sessions          int64   `json:"sessions"`
	Pageviews         int64   `json:"pageviews"`
	BounceRate        float64 `json:"bounceRate"`
	AvgDuration       int64   `json:"avgDuration"`
	ViewsPerSession   float64 `json:"viewsPerSession"`
}

func (q statsQuery) kpis(since, until int64) (kpis, error) {
	w, args := q.sessionScope(since, until)
	s, err := rowOf(db, `SELECT COUNT(*) sessions, COUNT(DISTINCT s.visitor_id) visitors,
		SUM(CASE WHEN s.pageviews <= 1 THEN 1 ELSE 0 END) bounces, AVG(s.last_seen - s.started_at) avg_duration
		FROM sessions s WHERE `+w, args...)
	if err != nil {
		return kpis{}, err
	}
	pw, pargs := q.pageviewScope(since, until)
	pv, err := rowOf(db, "SELECT COUNT(*) n FROM pageviews p JOIN sessions s ON s.id = p.session_id WHERE "+pw, pargs...)
	if err != nil {
		return kpis{}, err
	}
	k := kpis{
		Visitors: i64(s, "visitors"), VisitorsEstimated: q.isEstimate(since, until),
		Sessions: i64(s, "sessions"), Pageviews: i64(pv, "n"), AvgDuration: int64(f64(s, "avg_duration") + 0.5),
	}
	if k.Sessions > 0 {
		k.BounceRate = float64(i64(s, "bounces")) / float64(k.Sessions)
		k.ViewsPerSession = float64(k.Pageviews) / float64(k.Sessions)
	}
	return k, nil
}

type point struct {
	T         int64 `json:"t"`
	Visitors  int64 `json:"visitors"`
	Pageviews int64 `json:"pageviews"`
	Sessions  int64 `json:"sessions"`
}

func (q statsQuery) series(w window) (map[string]any, error) {
	off := q.TzOffset * 60_000
	bucket := func(col string) string {
		return fmt.Sprintf("((%s + %d) / %d) * %d - %d", col, off, w.bucket, w.bucket, off)
	}
	pw, pargs := q.pageviewScope(w.since, w.until)
	pvRows, err := rowsOf(db, `SELECT `+bucket("p.ts")+` b, COUNT(DISTINCT p.visitor_id) visitors, COUNT(*) pageviews
		FROM pageviews p JOIN sessions s ON s.id = p.session_id WHERE `+pw+` GROUP BY b`, pargs...)
	if err != nil {
		return nil, err
	}
	sw, sargs := q.sessionScope(w.since, w.until)
	sRows, err := rowsOf(db, `SELECT `+bucket("s.started_at")+` b, COUNT(*) sessions
		FROM sessions s WHERE `+sw+` AND s.started_at >= ? GROUP BY b`, append(sargs, w.since)...)
	if err != nil {
		return nil, err
	}

	points := []*point{}
	byT := map[int64]*point{}
	for t := floorDiv(w.since+off, w.bucket)*w.bucket - off; t <= w.until; t += w.bucket {
		p := &point{T: t}
		points = append(points, p)
		byT[t] = p
	}
	for _, r := range pvRows {
		if p := byT[i64(r, "b")]; p != nil {
			p.Visitors, p.Pageviews = i64(r, "visitors"), i64(r, "pageviews")
		}
	}
	for _, r := range sRows {
		if p := byT[i64(r, "b")]; p != nil {
			p.Sessions = i64(r, "sessions")
		}
	}
	name := "day"
	if w.bucket == hourMs {
		name = "hour"
	}
	return map[string]any{"bucket": name, "points": points}, nil
}

func (q statsQuery) breakdowns(w window, out map[string]any) error {
	const limit = 50
	pw, pargs := q.pageviewScope(w.since, w.until)
	sw, sargs := q.sessionScope(w.since, w.until)
	// Across all sites, the same path on two sites is two different pages, so pages carry their site.
	pageSite, pageGroup := "", ""
	if q.Sites.all {
		pageSite, pageGroup = "p.site_id site, ", "p.site_id, "
	}
	queries := map[string]struct {
		sql  string
		args []any
	}{
		"pages": {`SELECT ` + pageSite + `p.path name, COUNT(DISTINCT p.visitor_id) visitors, COUNT(*) pageviews, CAST(AVG(p.duration) AS INTEGER) avg_time
			FROM pageviews p JOIN sessions s ON s.id = p.session_id WHERE ` + pw + ` GROUP BY ` + pageGroup + `p.path ORDER BY visitors DESC, pageviews DESC`, pargs},
		"referrers": {`SELECT s.referrer name, COUNT(DISTINCT s.visitor_id) visitors, COUNT(*) sessions
			FROM sessions s WHERE ` + sw + ` AND s.referrer IS NOT NULL GROUP BY 1 ORDER BY visitors DESC`, sargs},
		"campaigns": {`SELECT s.utm_campaign name, COUNT(DISTINCT s.visitor_id) visitors, COUNT(*) sessions
			FROM sessions s WHERE ` + sw + ` AND s.utm_campaign IS NOT NULL GROUP BY 1 ORDER BY visitors DESC`, sargs},
	}
	for key, col := range map[string]string{
		"sources": "s.source", "entryPages": "s.entry_path", "exitPages": "s.exit_path",
		"devices": "s.device", "browsers": "s.browser", "os": "s.os", "countries": "s.country",
	} {
		sel, group := "coalesce("+col+", 'Unknown') name", "1"
		if q.Sites.all && (key == "entryPages" || key == "exitPages") {
			sel, group = "s.site_id site, "+sel, "1, 2"
		}
		queries[key] = struct {
			sql  string
			args []any
		}{`SELECT ` + sel + `, COUNT(DISTINCT s.visitor_id) visitors, COUNT(*) sessions
			FROM sessions s WHERE ` + sw + ` GROUP BY ` + group + ` ORDER BY visitors DESC, sessions DESC`, sargs}
	}
	if q.Sites.all {
		queries["sites"] = struct {
			sql  string
			args []any
		}{`SELECT s.site_id site, COUNT(DISTINCT s.visitor_id) visitors, COUNT(*) sessions
			FROM sessions s WHERE ` + sw + ` GROUP BY 1 ORDER BY visitors DESC, sessions DESC`, sargs}
	}
	ew, eargs := q.Sites.where("e.site_id")
	ew, eargs = q.filters(ew+" AND e.ts >= ? AND e.ts < ?", append(eargs, w.since, w.until))
	queries["events"] = struct {
		sql  string
		args []any
	}{`SELECT e.name name, COUNT(DISTINCT e.visitor_id) visitors, COUNT(*) count
		FROM events e JOIN sessions s ON s.id = e.session_id WHERE ` + ew + ` GROUP BY e.name ORDER BY visitors DESC`, eargs}

	for key, qq := range queries {
		rows, err := rowsOf(db, qq.sql+fmt.Sprintf(" LIMIT %d", limit), qq.args...)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		out[key] = rows
	}
	return nil
}

func overview(q statsQuery) (map[string]any, error) {
	now := time.Now().UnixMilli()
	w := windowFor(q, now)
	length := w.until - w.since

	// Rolling unique-visitor windows, each compared with the window before it.
	windows := []map[string]any{}
	for _, win := range []struct {
		key string
		ms  int64
	}{{"24h", dayMs}, {"3d", 3 * dayMs}, {"7d", 7 * dayMs}, {"30d", 30 * dayMs}} {
		windows = append(windows, map[string]any{
			"key":       win.key,
			"visitors":  q.uniques(now-win.ms, now),
			"previous":  q.uniques(now-2*win.ms, now-win.ms),
			"estimated": q.isEstimate(now-win.ms, now),
		})
	}
	cur, err := q.kpis(w.since, w.until)
	if err != nil {
		return nil, err
	}
	prev, err := q.kpis(w.since-length, w.since)
	if err != nil {
		return nil, err
	}
	series, err := q.series(w)
	if err != nil {
		return nil, err
	}
	// How many sessions the bot filter is hiding in this range (other filters still apply).
	withBots := q
	withBots.Bots = true
	bw, bargs := withBots.sessionScope(w.since, w.until)
	hidden, _ := rowOf(db, "SELECT COUNT(*) n FROM sessions s WHERE "+bw+" AND s.bot = 1", bargs...)

	out := map[string]any{
		"range": q.Range, "since": w.since, "until": w.until, "botSessions": i64(hidden, "n"), "bots": q.Bots,
		"windows": windows, "kpis": cur, "previous": prev, "series": series,
	}
	return out, q.breakdowns(w, out)
}

// ---- Sessions and visitors ----

// withJSON turns stored JSON text columns (traits, props) into raw JSON for the response.
func withJSON(r Row, cols ...string) Row {
	for _, c := range cols {
		if s := str(r, c); s != "" && json.Valid([]byte(s)) {
			r[c] = json.RawMessage(s)
		} else {
			r[c] = nil
		}
	}
	return r
}

func listSessions(sc siteScope, before int64, visitorID string, bots bool) ([]Row, error) {
	w, args := sc.where("s.site_id")
	where := []string{w}
	if !bots {
		where = append(where, "s.bot = 0")
	}
	if before > 0 {
		where, args = append(where, "s.started_at < ?"), append(args, before)
	}
	if visitorID != "" {
		where, args = append(where, "s.visitor_id = ?"), append(args, visitorID)
	}
	rows, err := rowsOf(db, `SELECT s.*, v.user_id, v.traits FROM sessions s
		LEFT JOIN visitors v ON v.site_id = s.site_id AND v.id = s.visitor_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY s.started_at DESC LIMIT 50`, args...)
	for _, r := range rows {
		withJSON(r, "traits")
	}
	return rows, err
}

func sessionDetail(sc siteScope, id string) (map[string]any, error) {
	w, args := sc.where("s.site_id")
	s, err := rowOf(db, `SELECT s.*, v.user_id, v.traits, v.first_seen visitor_first_seen,
		(SELECT COUNT(*) FROM sessions x WHERE x.site_id = s.site_id AND x.visitor_id = s.visitor_id AND x.started_at <= s.started_at) visit_number
		FROM sessions s LEFT JOIN visitors v ON v.site_id = s.site_id AND v.id = s.visitor_id
		WHERE `+w+` AND s.id = ?`, append(args, id)...)
	if err != nil || s == nil {
		return nil, err
	}
	pageviews, err := rowsOf(db, "SELECT id, ts, path, title, duration FROM pageviews WHERE session_id = ? ORDER BY ts", id)
	if err != nil {
		return nil, err
	}
	events, err := rowsOf(db, "SELECT id, ts, name, path, props FROM events WHERE session_id = ? ORDER BY ts", id)
	for _, e := range events {
		withJSON(e, "props")
	}
	s["bot_reasons"] = botLabels(i64(s, "bot_signals"))
	s["bot_threshold"] = botThreshold
	return map[string]any{"session": withJSON(s, "traits"), "pageviews": pageviews, "events": events}, err
}

func listVisitors(sc siteScope, before int64, search string, identified, bots bool) ([]Row, error) {
	w, args := sc.where("v.site_id")
	where := []string{w}
	if !bots {
		// Hide visitors whose every session looks like a bot.
		where = append(where, "EXISTS (SELECT 1 FROM sessions b WHERE b.site_id = v.site_id AND b.visitor_id = v.id AND b.bot = 0)")
	}
	if before > 0 {
		where, args = append(where, "v.last_seen < ?"), append(args, before)
	}
	if identified {
		where = append(where, "v.user_id IS NOT NULL")
	}
	if search != "" {
		where = append(where, "(v.user_id LIKE ? OR v.traits LIKE ? OR v.id LIKE ?)")
		args = append(args, "%"+search+"%", "%"+search+"%", search+"%")
	}
	// Pick the page of visitors first, then fill in their columns. Across several sites the sort can't use the
	// (site_id, last_seen) index, and SQLite would otherwise run these subqueries for every visitor before sorting.
	rows, err := rowsOf(db, `SELECT v.*,
		(SELECT COUNT(*) FROM sessions s WHERE s.site_id = v.site_id AND s.visitor_id = v.id) sessions,
		(SELECT SUM(pageviews) FROM sessions s WHERE s.site_id = v.site_id AND s.visitor_id = v.id) pageviews,
		(SELECT source FROM sessions s WHERE s.site_id = v.site_id AND s.visitor_id = v.id ORDER BY started_at LIMIT 1) first_source,
		(SELECT device || ' · ' || browser FROM sessions s WHERE s.site_id = v.site_id AND s.visitor_id = v.id ORDER BY started_at DESC LIMIT 1) last_device,
		(SELECT country FROM sessions s WHERE s.site_id = v.site_id AND s.visitor_id = v.id ORDER BY started_at DESC LIMIT 1) last_country
		FROM (SELECT * FROM visitors v WHERE `+strings.Join(where, " AND ")+` ORDER BY v.last_seen DESC LIMIT 50) v
		ORDER BY v.last_seen DESC`, args...)
	for _, r := range rows {
		withJSON(r, "traits")
	}
	return rows, err
}

func visitorDetail(sc siteScope, id string) (map[string]any, error) {
	w, args := sc.where("site_id")
	v, err := rowOf(db, "SELECT * FROM visitors WHERE "+w+" AND id = ?", append(args, id)...)
	if err != nil || v == nil {
		return nil, err
	}
	// The rest is about this visitor's own site, even when the request covers every site.
	siteID := i64(v, "site_id")
	// An identified user may have several browsers (visitor ids); show them together.
	devices := []Row{{"id": v["id"], "first_seen": v["first_seen"], "last_seen": v["last_seen"]}}
	if uid := str(v, "user_id"); uid != "" {
		if devices, err = rowsOf(db, "SELECT id, first_seen, last_seen FROM visitors WHERE site_id = ? AND user_id = ? ORDER BY last_seen DESC", siteID, uid); err != nil {
			return nil, err
		}
	}
	ids := []any{siteID}
	for _, d := range devices {
		ids = append(ids, d["id"])
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(devices)), ",")
	sessions, err := rowsOf(db, "SELECT * FROM sessions WHERE site_id = ? AND visitor_id IN ("+in+") ORDER BY started_at DESC LIMIT 200", ids...)
	if err != nil {
		return nil, err
	}
	totals, err := rowOf(db, "SELECT COUNT(*) sessions, SUM(pageviews) pageviews, SUM(last_seen - started_at) time FROM sessions WHERE site_id = ? AND visitor_id IN ("+in+")", ids...)
	return map[string]any{"visitor": withJSON(v, "traits"), "devices": devices, "sessions": sessions, "totals": totals}, err
}

func validRange(r string) string {
	if slices.Contains(ranges, r) {
		return r
	}
	return "7d"
}
