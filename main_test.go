package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Route patterns that overlap panic at registration, so make sure the mux builds.
func TestRoutesRegister(t *testing.T) {
	routes()
}

func TestSplitDomains(t *testing.T) {
	got := splitDomains("https://www.Example.com/, app.example.com:8080  example.com")
	if want := []string{"example.com", "app.example.com"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHostAllowed(t *testing.T) {
	s := Site{Domains: []string{"example.com"}}
	for host, want := range map[string]bool{
		"example.com": true, "www.example.com": true, "shop.example.com": true,
		"badexample.com": false, "example.com.evil.test": false,
	} {
		if got := hostAllowed(s, host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	if !hostAllowed(Site{}, "anything.test") {
		t.Error("a site with no domains should accept any host")
	}
}

func TestClassifySource(t *testing.T) {
	for _, c := range []struct{ ref, utm, want string }{
		{"", "", "Direct"},
		{"https://www.google.co.uk/search", "", "Google"},
		{"https://t.co/abc", "", "X (Twitter)"},
		{"https://blog.example.org/post", "", "blog.example.org"},
		{"https://www.google.com/", "newsletter", "newsletter"},
	} {
		if got := classifySource(c.ref, c.utm); got != c.want {
			t.Errorf("classifySource(%q, %q) = %q, want %q", c.ref, c.utm, got, c.want)
		}
	}
}

func TestParseUA(t *testing.T) {
	iphone := parseUA("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1", 0)
	if iphone != (agent{"Safari", "iOS", "Mobile"}) {
		t.Errorf("iPhone parsed as %+v", iphone)
	}
	edge := parseUA("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36 Edg/140.0", 1920)
	if edge != (agent{"Edge", "Windows", "Desktop"}) {
		t.Errorf("Edge parsed as %+v", edge)
	}
	if !isBot("Mozilla/5.0 (compatible; Googlebot/2.1)") || isBot(edge.Browser+" Mozilla/5.0") {
		t.Error("bot detection")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword("correct horse", h) || verifyPassword("wrong", h) {
		t.Error("password verification")
	}
}

func TestBotScore(t *testing.T) {
	// A data-centre visit that never interacts is a bot; a VPN user who scrolls is not.
	dc := sigDataCenter.bit | sigNoInteraction.bit
	if s := botScore(dc, false); s < botThreshold {
		t.Errorf("data centre, no interaction: score %d, want >= %d", s, botThreshold)
	}
	if s := botScore(sigDataCenter.bit, true); s >= botThreshold {
		t.Errorf("data centre with interaction: score %d, want < %d", s, botThreshold)
	}
	// One weak signal on its own (e.g. a reader who never scrolls) is not enough.
	if s := botScore(sigNoInteraction.bit|sigNoAcceptLanguage.bit, false); s >= botThreshold {
		t.Errorf("weak signals: score %d, want < %d", s, botThreshold)
	}
	if !fastNavigation(5, 0, 5_000) || fastNavigation(5, 0, 60_000) || fastNavigation(2, 0, 100) {
		t.Error("fastNavigation")
	}
}

func TestIsDataCenter(t *testing.T) {
	for org, want := range map[string]bool{
		"Amazon.com, Inc.": true, "DigitalOcean, LLC": true, "Hetzner Online GmbH": true, "Microsoft Corporation": true,
		"Comcast Cable Communications, LLC": false, "Deutsche Telekom AG": false,
		"Akamai Technologies, Inc.": false, "Cloudflare, Inc.": false, "": false, // iCloud Private Relay / WARP exits
	} {
		if got := isDataCenter(org); got != want {
			t.Errorf("isDataCenter(%q) = %v, want %v", org, got, want)
		}
	}
}

func TestNormalizePublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"analytics.example.com":          "https://analytics.example.com",
		"https://Analytics.Example.com/": "https://analytics.example.com",
		"http://localhost:3300":          "http://localhost:3300",
		"https://example.com/omega/":     "https://example.com/omega",
		"":                               "",
	} {
		if got, err := normalizePublicURL(in); err != nil || got != want {
			t.Errorf("normalizePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://example.com", "https://example.com/?x=1", "https://user@example.com", "https://"} {
		if _, err := normalizePublicURL(bad); err == nil {
			t.Errorf("normalizePublicURL(%q) should fail", bad)
		}
	}
}

func TestMinifiedTracker(t *testing.T) {
	src, err := assets.ReadFile("tracker/t.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := minifyJS(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > len(src)*6/10 {
		t.Errorf("minified tracker is %d bytes from %d; expected at least 40%% off", len(out), len(src))
	}
	// Public API and payload keys must survive minification.
	for _, s := range []string{"omega", "track", "identify", "consent", "data-site", "sendBeacon", "/api/collect"} {
		if !bytes.Contains(out, []byte(s)) {
			t.Errorf("minified tracker lost %q", s)
		}
	}
}

// "All sites" adds up every site, and keeps the same path on two sites apart.
func TestAllSitesOverview(t *testing.T) {
	if err := openDB(filepath.Join(t.TempDir(), "omega.db")); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := createSite("A", "a.test", "cookies")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createSite("B", "b.test", "cookies")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i, s := range []struct {
		site    Site
		id      string
		visitor string
	}{{a, "s1", "v1"}, {a, "s2", "v2"}, {b, "s3", "v3"}} {
		ts := now - int64(i+1)*60_000
		db.Exec(`INSERT INTO sessions (id, site_id, visitor_id, started_at, last_seen, entry_path, exit_path, pageviews, source)
			VALUES (?, ?, ?, ?, ?, '/', '/', 1, 'Direct')`, s.id, s.site.ID, s.visitor, ts, ts)
		db.Exec("INSERT INTO pageviews (id, site_id, session_id, visitor_id, ts, path) VALUES (?, ?, ?, ?, ?, '/')", s.id, s.site.ID, s.id, s.visitor, ts)
		db.Exec("INSERT INTO visitors (site_id, id, first_seen, last_seen) VALUES (?, ?, ?, ?)", s.site.ID, s.visitor, ts, ts)
	}

	one, err := overview(statsQuery{Sites: siteScope{ids: []int64{a.ID}}, Range: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	if k := one["kpis"].(kpis); k.Sessions != 2 || k.Pageviews != 2 {
		t.Errorf("site A: %+v", k)
	}
	if _, ok := one["sites"]; ok {
		t.Error("a single site should not get a per-site breakdown")
	}

	all, err := overview(statsQuery{Sites: siteScope{ids: []int64{a.ID, b.ID}, all: true}, Range: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	if k := all["kpis"].(kpis); k.Sessions != 3 || k.Visitors != 3 || k.Pageviews != 3 {
		t.Errorf("all sites: %+v", k)
	}
	if pages := all["pages"].([]Row); len(pages) != 2 {
		t.Errorf("all sites: want / on each site as its own page, got %v", pages)
	}
	if sites := all["sites"].([]Row); len(sites) != 2 || i64(sites[0], "site") != a.ID || i64(sites[0], "visitors") != 2 {
		t.Errorf("all sites: per-site breakdown %v", sites)
	}

	rows, err := listSessions(siteScope{ids: []int64{a.ID, b.ID}, all: true}, 0, "", false)
	if err != nil || len(rows) != 3 {
		t.Errorf("all-sites sessions: %d rows, %v", len(rows), err)
	}
	v, err := visitorDetail(siteScope{ids: []int64{a.ID, b.ID}, all: true}, "v3")
	if err != nil || v == nil || i64(v["visitor"].(Row), "site_id") != b.ID {
		t.Errorf("all-sites visitor detail: %v, %v", v, err)
	}
}
