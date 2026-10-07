package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/js"
)

// The tracker is kept readable in tracker/t.js. Deployed builds (anything with a version, i.e. release binaries
// and container images) serve it minified and gzip-compressed; local "dev" builds serve the readable source.
// OMEGA_TRACKER_MINIFY=on|off overrides that.

type trackerAsset struct {
	plain []byte // what's served without compression
	gzip  []byte
	etag  string
}

const (
	trackerBanner = "/*! Omega Analytics tracker: https://github.com/timothydodd/project-omega */\n"
	replayBanner  = "/*! Omega Analytics session replay: https://github.com/timothydodd/project-omega. Includes rrweb (MIT): https://github.com/rrweb-io/rrweb */\n"
)

func loadTracker() trackerAsset {
	return newTrackerAsset(minifiedScript("tracker/t.js", trackerBanner, nil))
}

// loadReplayScript builds /replay.js: rrweb's recorder, then tracker/replay.js. rrweb's bundle runs with
// define/exports/module hidden, so it can't register itself with a page's module loader or add globals.
func loadReplayScript() trackerAsset {
	vendor, err := assets.ReadFile("tracker/vendor/rrweb-record.min.js")
	if err != nil {
		log.Fatal(err)
	}
	return newTrackerAsset(minifiedScript("tracker/replay.js", replayBanner, func(glue []byte) []byte {
		var b bytes.Buffer
		b.WriteString("(function(){var o={};(function(define,exports,module){\n")
		b.Write(vendor)
		b.WriteString("\n}).call(o);var rrwebRecord=o.rrwebRecord;\n")
		b.Write(glue)
		b.WriteString("\n})();\n")
		return b.Bytes()
	}))
}

// minifiedScript reads an embedded script, minifies it when minifyTracker() says so, and lets wrap add to it.
func minifiedScript(name, banner string, wrap func([]byte) []byte) []byte {
	src, err := assets.ReadFile(name)
	if err != nil {
		log.Fatal(err)
	}
	body := src
	if minifyTracker() {
		if out, err := minifyJS(src); err != nil {
			log.Printf("%s minify failed, serving the readable source: %v", name, err)
		} else {
			body = out
			log.Printf("%s minified: %d → %d bytes", name, len(src), len(out))
		}
	}
	if wrap != nil {
		body = wrap(body)
	}
	return append([]byte(banner), body...)
}

func newTrackerAsset(body []byte) trackerAsset {
	var gz bytes.Buffer
	w, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	w.Write(body)
	w.Close()
	sum := sha256.Sum256(body)
	return trackerAsset{plain: body, gzip: gz.Bytes(), etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`}
}

func minifyTracker() bool {
	switch strings.ToLower(os.Getenv("OMEGA_TRACKER_MINIFY")) {
	case "on", "1", "true":
		return true
	case "off", "0", "false":
		return false
	}
	return version != "dev"
}

func minifyJS(src []byte) ([]byte, error) {
	m := minify.New()
	m.Add("text/javascript", &js.Minifier{})
	return m.Bytes("text/javascript", src)
}

func (t trackerAsset) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("ETag", t.etag)
	h.Set("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, t.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.Write(t.gzip)
		return
	}
	w.Write(t.plain)
}
