package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// seed fills a "Demo site" with ~90 days of realistic-looking traffic so the dashboard has something to show.
// Usage: omega seed
func seed() error {
	sites, err := listSites()
	if err != nil {
		return err
	}
	var site Site
	for _, s := range sites {
		if s.Name == "Demo site" {
			site = s
		}
	}
	if site.ID == 0 {
		// The demo data has returning visitors across days, which only cookie-based ids can see.
		if site, err = createSite(siteInput{Name: "Demo site", Domains: "localhost, 127.0.0.1", Privacy: "consent"}); err != nil {
			return err
		}
	} else if r, _ := rowOf(db, "SELECT 1 x FROM sessions WHERE site_id = ? LIMIT 1", site.ID); r != nil {
		fmt.Printf("\"Demo site\" already has data. Site key: %s\n", site.Key)
		return nil
	}

	type page struct{ path, title string }
	pages := weighted[page]{
		{page{"/", "Home"}, 40}, {page{"/pricing", "Pricing"}, 18}, {page{"/features", "Features"}, 14}, {page{"/blog", "Blog"}, 10},
		{page{"/blog/launch-week", "Launch week recap"}, 8}, {page{"/blog/first-party-cookies", "Why first-party cookies"}, 6},
		{page{"/docs", "Docs"}, 9}, {page{"/docs/getting-started", "Getting started"}, 7}, {page{"/about", "About"}, 4},
		{page{"/contact", "Contact"}, 3}, {page{"/signup", "Sign up"}, 6}, {page{"/login", "Log in"}, 5},
	}
	type src struct{ source, referrer, utmSource, utmMedium, utmCampaign string }
	sources := weighted[src]{
		{src{"Direct", "", "", "", ""}, 34}, {src{"Google", "https://www.google.com/", "", "", ""}, 30},
		{src{"Bing", "https://www.bing.com/", "", "", ""}, 4}, {src{"DuckDuckGo", "https://duckduckgo.com/", "", "", ""}, 3},
		{src{"LinkedIn", "https://www.linkedin.com/", "", "", ""}, 6}, {src{"X (Twitter)", "https://t.co/abc", "", "", ""}, 5},
		{src{"Reddit", "https://www.reddit.com/r/webdev/", "", "", ""}, 4}, {src{"Hacker News", "https://news.ycombinator.com/", "", "", ""}, 3},
		{src{"ChatGPT", "https://chatgpt.com/", "", "", ""}, 3}, {src{"GitHub", "https://github.com/", "", "", ""}, 2},
		{src{"newsletter", "", "newsletter", "email", "september-update"}, 4},
		{src{"devblog.example.com", "https://devblog.example.com/posts/analytics", "", "", ""}, 2},
	}
	type agentInfo struct{ browser, os, device, screen string }
	agents := weighted[agentInfo]{
		{agentInfo{"Chrome", "Windows", "Desktop", "1920x1080"}, 30}, {agentInfo{"Chrome", "macOS", "Desktop", "1512x982"}, 12},
		{agentInfo{"Safari", "macOS", "Desktop", "1728x1117"}, 8}, {agentInfo{"Edge", "Windows", "Desktop", "1920x1080"}, 9},
		{agentInfo{"Firefox", "Windows", "Desktop", "2560x1440"}, 4}, {agentInfo{"Safari", "iOS", "Mobile", "393x852"}, 20},
		{agentInfo{"Chrome", "Android", "Mobile", "412x915"}, 13}, {agentInfo{"Safari", "iOS", "Tablet", "820x1180"}, 3},
	}
	countries := weighted[string]{{"US", 45}, {"GB", 10}, {"CA", 8}, {"DE", 7}, {"IN", 7}, {"AU", 4}, {"FR", 4}, {"BR", 3}, {"NL", 3}}
	hours := weighted[int64]{}
	for h, w := range []float64{1, 1, 1, 1, 1, 1, 1, 2, 3, 5, 6, 6, 5, 6, 7, 7, 6, 5, 4, 4, 4, 3, 2, 1} {
		hours = append(hours, item[int64]{int64(h), w})
	}
	pageCounts := weighted[int]{{1, 38}, {2, 22}, {3, 16}, {4, 10}, {5, 7}, {6, 4}, {8, 3}}
	names := []string{"Ada", "Grace", "Linus", "Margaret", "Alan", "Katherine", "Dennis", "Barbara"}

	type visitor struct {
		id, userID string
		agent      agentInfo
		country    string
	}
	var visitors []*visitor
	now := time.Now().UnixMilli()
	sessionCount, pvCount := 0, 0

	err = withTx(func(tx *sql.Tx) error {
		exec := func(q string, args ...any) {
			if err == nil {
				_, err = tx.Exec(q, args...)
			}
		}
		event := func(sid string, v *visitor, ts int64, name, path string, props map[string]string) {
			var p any
			if props != nil {
				b, _ := json.Marshal(props)
				p = string(b)
			}
			exec("INSERT INTO events (site_id, session_id, visitor_id, ts, name, path, props) VALUES (?, ?, ?, ?, ?, ?, ?)",
				site.ID, sid, v.id, ts, name, path, p)
		}
		for d := int64(89); d >= 0; d-- {
			dayStart := floorDiv(now-d*dayMs, dayMs) * dayMs
			weekday := time.UnixMilli(dayStart).UTC().Weekday()
			growth := 1 + float64(89-d)/120 // slowly growing traffic
			weekend := 1.0
			if weekday == time.Saturday || weekday == time.Sunday {
				weekend = 0.6
			}
			spike := map[int64]float64{12: 2.6, 11: 1.5}[d] // a Hacker News moment
			if spike == 0 {
				spike = 1
			}
			sessionsToday := int((90 + rand.Float64()*40) * growth * weekend * spike)

			for range sessionsToday {
				start := dayStart + hours.pick()*hourMs + rand.Int64N(hourMs)
				if start > now {
					continue
				}
				var v *visitor
				if len(visitors) > 50 && rand.Float64() < 0.35 {
					v = visitors[rand.IntN(len(visitors))]
				} else {
					v = &visitor{id: newID(), agent: agents.pick(), country: countries.pick()}
					visitors = append(visitors, v)
					exec("INSERT INTO visitors (site_id, id, first_seen, last_seen) VALUES (?, ?, ?, ?)", site.ID, v.id, start, start)
				}
				s := sources.pick()
				if d == 12 && rand.Float64() < 0.55 {
					s = src{"Hacker News", "https://news.ycombinator.com/", "", "", ""}
				}
				sid := newID()
				path := []page{pages.pick()}
				if s.source == "Google" && rand.Float64() < 0.4 {
					for !strings.HasPrefix(path[0].path, "/blog") && !strings.HasPrefix(path[0].path, "/docs") {
						path[0] = pages.pick()
					}
				}
				for n := pageCounts.pick(); len(path) < n; {
					path = append(path, pages.pick())
				}

				t := start
				for _, p := range path {
					spread := 70_000.0
					if strings.HasPrefix(p.path, "/blog/") {
						spread = 240_000
					}
					duration := int64(4_000 + rand.Float64()*spread)
					exec("INSERT INTO pageviews (id, site_id, session_id, visitor_id, ts, path, title, duration) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
						newID(), site.ID, sid, v.id, t, p.path, p.title, duration)
					switch {
					case p.path == "/signup" && rand.Float64() < 0.45:
						event(sid, v, t+20_000, "Signup", p.path, map[string]string{"plan": weighted[string]{{"free", 6}, {"pro", 3}, {"team", 1}}.pick()})
						if v.userID == "" {
							v.userID = "user_" + strings.ToLower(newID()[:8])
							first := names[rand.IntN(len(names))]
							traits, _ := json.Marshal(map[string]string{"name": first, "email": strings.ToLower(first) + "." + v.userID[5:9] + "@example.com"})
							exec("UPDATE visitors SET user_id = ?, traits = ? WHERE site_id = ? AND id = ?", v.userID, string(traits), site.ID, v.id)
						}
					case p.path == "/pricing" && rand.Float64() < 0.3:
						event(sid, v, t+9_000, "Pricing toggle", p.path, map[string]string{"billing": weighted[string]{{"yearly", 1}, {"monthly", 1}}.pick()})
					case p.path == "/contact" && rand.Float64() < 0.3:
						event(sid, v, t+30_000, "Contact form sent", p.path, nil)
					}
					t += duration + 1_000
					pvCount++
				}
				last := min(t, now)
				exec(`INSERT INTO sessions (id, site_id, visitor_id, id_method, started_at, last_seen, entry_path, exit_path, exit_title, pageviews,
						referrer, source, utm_source, utm_medium, utm_campaign, browser, os, device, country, language, timezone, screen)
					VALUES (?, ?, ?, 'cookie', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'en-US', 'America/New_York', ?)`,
					sid, site.ID, v.id, start, last, path[0].path, path[len(path)-1].path, path[len(path)-1].title, len(path),
					nullable(s.referrer), s.source, nullable(s.utmSource), nullable(s.utmMedium), nullable(s.utmCampaign),
					v.agent.browser, v.agent.os, v.agent.device, v.country, v.agent.screen)
				exec("UPDATE visitors SET last_seen = max(last_seen, ?) WHERE site_id = ? AND id = ?", last, site.ID, v.id)
				sessionCount++
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	fmt.Printf("Seeded \"Demo site\": %d visitors, %d sessions, %d page views.\n", len(visitors), sessionCount, pvCount)
	fmt.Printf("Site key: %s\n", site.Key)
	fmt.Printf("Open /demo?site=%s&cookies=consent in a few tabs to see yourself on the Live view.\n", site.Key)
	return nil
}

type item[T any] struct {
	v T
	w float64
}

type weighted[T any] []item[T]

func (ws weighted[T]) pick() T {
	total := 0.0
	for _, x := range ws {
		total += x.w
	}
	r := rand.Float64() * total
	for _, x := range ws {
		if r -= x.w; r <= 0 {
			return x.v
		}
	}
	return ws[0].v
}
