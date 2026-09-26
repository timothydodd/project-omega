// Omega Analytics: lightweight, self-hosted, first-party web analytics in a single binary.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed public tracker/t.js
var assets embed.FS

const maxBody = 32 << 10

func main() {
	dbPath := env("OMEGA_DB", "data/omega.db")
	if err := openDB(dbPath); err != nil {
		log.Fatalf("open database %s: %v", dbPath, err)
	}
	if err := reloadSites(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case "seed":
			err = seed()
		case "geoip-update":
			err = updateIPDatabases(dbPath)
		default:
			err = fmt.Errorf("unknown command %q (commands: seed, geoip-update)", os.Args[1])
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	openIPDatabases(dbPath)
	go autoUpdateIPDatabases(dbPath)

	live.restore()
	go live.run()

	addr := env("HOST", "0.0.0.0") + ":" + env("PORT", "3300")
	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("Omega Analytics running at http://localhost:%s", env("PORT", "3300"))
	if !hasUsers() {
		log.Print("No account yet: open the dashboard to create one.")
	}
	log.Fatal(srv.ListenAndServe())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func routes() http.Handler {
	dbPath := env("OMEGA_DB", "data/omega.db")
	mux := http.NewServeMux()
	public, _ := fs.Sub(assets, "public")

	// Liveness/readiness probe for Kubernetes.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			fail(w, 503, "database unavailable")
			return
		}
		w.Write([]byte("ok"))
	})

	// ---- Public: tracker + ingest (called cross-origin from tracked sites) ----
	mux.HandleFunc("GET /t.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		b, _ := assets.ReadFile("tracker/t.js")
		w.Write(b)
	})
	mux.HandleFunc("POST /api/collect", handleCollect)
	mux.HandleFunc("OPTIONS /api/collect", handleCollect)
	serveDemo := func(w http.ResponseWriter, r *http.Request) { serveAsset(w, public, "demo.html") }
	mux.HandleFunc("GET /demo", serveDemo)
	mux.HandleFunc("GET /demo/", serveDemo)

	// ---- Auth ----
	mux.HandleFunc("GET /api/auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"user": currentUser(r), "needsSetup": !hasUsers()})
	})
	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		if hasUsers() {
			fail(w, 409, "An account already exists. Sign in instead.")
			return
		}
		var body struct{ Email, Password string }
		if !readJSON(w, r, &body) {
			return
		}
		if !regexp.MustCompile(`^\S+@\S+$`).MatchString(body.Email) {
			fail(w, 400, "Enter a valid email address.")
			return
		}
		if len(body.Password) < 8 {
			fail(w, 400, "Use a password of at least 8 characters.")
			return
		}
		if err := createUser(body.Email, body.Password); err != nil {
			serverError(w, err)
			return
		}
		login(w, r, body.Email, body.Password)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Email, Password string }
		if !readJSON(w, r, &body) {
			return
		}
		if !login(w, r, body.Email, body.Password) {
			fail(w, 401, "Email or password is incorrect.")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		logout(w, r)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// ---- Dashboard API (signed in) ----
	api := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if currentUser(r) == nil {
				fail(w, 401, "Sign in required")
				return
			}
			h(w, r)
		})
	}
	api("GET /api/sites", func(w http.ResponseWriter, r *http.Request) {
		sites, err := listSites()
		respond(w, sites, err)
	})
	api("POST /api/sites", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name, Domains, Privacy string }
		if !readJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			fail(w, 400, "Give the site a name.")
			return
		}
		s, err := createSite(body.Name, body.Domains, body.Privacy)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 201, s)
	})
	api("PUT /api/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if _, ok := getSite(id); !ok {
			fail(w, 404, "Site not found")
			return
		}
		var body struct{ Name, Domains, Privacy string }
		if !readJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			fail(w, 400, "Give the site a name.")
			return
		}
		s, err := updateSite(id, body.Name, body.Domains, body.Privacy)
		respond(w, s, err)
	})
	api("DELETE /api/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if _, ok := getSite(id); !ok {
			fail(w, 404, "Site not found")
			return
		}
		respond(w, map[string]bool{"ok": true}, deleteSite(id))
	})

	api("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, settingsResponse())
	})
	api("POST /api/settings/ip-databases/update", func(w http.ResponseWriter, r *http.Request) {
		results := updateIPDatabasesNow(filepath.Dir(dbPath))
		writeJSON(w, 200, map[string]any{"results": results, "settings": settingsResponse()})
	})
	api("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ PublicURL string }
		if !readJSON(w, r, &body) {
			return
		}
		if _, fromEnv := publicURL(); fromEnv {
			fail(w, 409, "The address is set by the OMEGA_PUBLIC_URL environment variable on the server. Change it there.")
			return
		}
		u, err := normalizePublicURL(body.PublicURL)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		if err := setSetting("public_url", u); err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 200, settingsResponse())
	})

	api("GET /api/stats", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		q := r.URL.Query()
		tz, _ := strconv.ParseInt(q.Get("tz"), 10, 64)
		out, err := overview(statsQuery{
			SiteID: s.ID, Range: validRange(q.Get("range")), TzOffset: min(max(tz, -840), 840),
			Path: q.Get("path"), Source: q.Get("source"), Country: q.Get("country"), Bots: q.Get("bots") == "1",
		})
		if out != nil {
			out["geoip"] = countryDB.ready()
		}
		respond(w, out, err)
	}))
	api("GET /api/live", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		writeJSON(w, 200, live.snapshot(s.ID))
	}))
	api("GET /api/live/stream", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		live.serveStream(w, r, s.ID)
	}))
	api("GET /api/sessions", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		rows, err := listSessions(s.ID, before, r.URL.Query().Get("visitor"), r.URL.Query().Get("bots") == "1")
		respond(w, rows, err)
	}))
	api("GET /api/sessions/{id}", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		detail, err := sessionDetail(s.ID, r.PathValue("id"))
		if err == nil && detail == nil {
			fail(w, 404, "Session not found")
			return
		}
		respond(w, detail, err)
	}))
	api("GET /api/visitors", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		q := r.URL.Query()
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		rows, err := listVisitors(s.ID, before, q.Get("q"), q.Get("identified") == "1", q.Get("bots") == "1")
		respond(w, rows, err)
	}))
	api("GET /api/visitors/{id}", withSite(func(w http.ResponseWriter, r *http.Request, s Site) {
		detail, err := visitorDetail(s.ID, r.PathValue("id"))
		if err == nil && detail == nil {
			fail(w, 404, "Visitor not found")
			return
		}
		respond(w, detail, err)
	}))
	api("GET /api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "Not found") })

	// ---- Dashboard static files (single-page app) ----
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, public, strings.TrimPrefix(r.URL.Path, "/"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { serveAsset(w, public, "index.html") })
	return mux
}

func handleCollect(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, 413, "Payload too large")
		return
	}
	if e := collect(r, body); e != nil {
		if e.status == 500 {
			log.Printf("collect: %s", e.msg)
			e.msg = "Server error"
		}
		fail(w, e.status, e.msg)
		return
	}
	w.WriteHeader(202)
}

func withSite(h func(http.ResponseWriter, *http.Request, Site)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("site"), 10, 64)
		s, ok := getSite(id)
		if !ok {
			fail(w, 404, "Site not found")
			return
		}
		h(w, r, s)
	}
}

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon",
}

func serveAsset(w http.ResponseWriter, files fs.FS, name string) {
	b, err := fs.ReadFile(files, name)
	if err != nil {
		fail(w, 404, "Not found")
		return
	}
	ext := name[strings.LastIndex(name, "."):]
	if t, ok := contentTypes[ext]; ok {
		w.Header().Set("Content-Type", t)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	log.Print(err)
	fail(w, 500, "Server error")
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		fail(w, 400, "Invalid JSON")
		return false
	}
	return true
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
