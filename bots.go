package main

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Bot detection. Obvious bots (known crawler user agents, automation flags) are dropped at ingest. Everything
// else gets a score from weak signals that are rarely all present for a person; sessions at or above
// botThreshold are hidden from reports by default (kept in the database, and viewable with a toggle).
//
// Nothing here fingerprints anyone: signals are checked at ingest and only the resulting bits are stored.

type botSignal struct {
	bit    int64
	weight int64
	label  string // shown on the session page
}

var (
	sigHeadlessWindow   = botSignal{1 << 0, 40, "Browser window has no size (headless browser)"}
	sigNoLanguages      = botSignal{1 << 1, 30, "Browser reports no languages"}
	sigSoftwareRenderer = botSignal{1 << 2, 30, "Graphics are software-rendered (no GPU), typical of headless browsers"}
	sigNoPlugins        = botSignal{1 << 3, 20, "Desktop Chrome with no built-in plugins"}
	sigNoChromeObject   = botSignal{1 << 4, 30, "Claims to be Chrome but lacks Chrome's browser features"}
	sigDataCenter       = botSignal{1 << 5, 50, "Visit came from a data-centre / cloud network"}
	sigNoAcceptLanguage = botSignal{1 << 6, 30, "Request had no Accept-Language header"}
	sigNoFetchMetadata  = botSignal{1 << 7, 25, "Request lacked headers every modern browser sends"}
	sigFastNavigation   = botSignal{1 << 8, 30, "Pages were opened faster than a person could read them"}
	sigHighVolume       = botSignal{1 << 9, 40, "Many new visits from the same IP in a few minutes"}
	sigNoInteraction    = botSignal{1 << 10, 15, "No scrolling, clicking, tapping or typing"}

	botSignals = []botSignal{sigHeadlessWindow, sigNoLanguages, sigSoftwareRenderer, sigNoPlugins, sigNoChromeObject,
		sigDataCenter, sigNoAcceptLanguage, sigNoFetchMetadata, sigFastNavigation, sigHighVolume, sigNoInteraction}
)

const (
	botThreshold = 60
	// A real scroll, click, tap or keypress is good evidence of a person, though automation can fake it.
	interactionCredit = 30
)

// Bits the tracker reports about the browser (the "bs" field), mapped to server-side signals.
const (
	clientHeadlessWindow = 1 << iota
	clientNoLanguages
	clientSoftwareRenderer
	clientNoPlugins
	clientNoChromeObject
)

func botScore(signals int64, interacted bool) int64 {
	var score int64
	for _, s := range botSignals {
		if signals&s.bit != 0 {
			score += s.weight
		}
	}
	if interacted {
		score -= interactionCredit
	}
	return max(score, 0)
}

func botLabels(signals int64) []string {
	out := []string{}
	for _, s := range botSignals {
		if signals&s.bit != 0 {
			out = append(out, s.label)
		}
	}
	return out
}

// clientSignals maps what the tracker reported about the browser to signals.
func clientSignals(clientBits int64, browser, device string) int64 {
	var sig int64
	if clientBits&clientHeadlessWindow != 0 {
		sig |= sigHeadlessWindow.bit
	}
	if clientBits&clientNoLanguages != 0 {
		sig |= sigNoLanguages.bit
	}
	if clientBits&clientSoftwareRenderer != 0 {
		sig |= sigSoftwareRenderer.bit
	}
	// Desktop Chrome always ships built-in PDF plugins; mobile browsers legitimately have none.
	if clientBits&clientNoPlugins != 0 && browser == "Chrome" && device == "Desktop" {
		sig |= sigNoPlugins.bit
	}
	if clientBits&clientNoChromeObject != 0 {
		sig |= sigNoChromeObject.bit
	}
	return sig
}

// requestSignals are checked on each page view: what the browser reported plus the request itself.
func requestSignals(r *http.Request, clientBits int64, ag agent, network string, newSession bool, ip string) int64 {
	sig := clientSignals(clientBits, ag.Browser, ag.Device)
	if isDataCenter(network) {
		sig |= sigDataCenter.bit
	}
	if r.Header.Get("Accept-Language") == "" {
		sig |= sigNoAcceptLanguage.bit
	}
	// Chromium browsers and Firefox send Sec-Fetch-* on every request, including sendBeacon.
	if (ag.Browser == "Chrome" || ag.Browser == "Edge" || ag.Browser == "Firefox") && r.Header.Get("Sec-Fetch-Mode") == "" {
		sig |= sigNoFetchMetadata.bit
	}
	if newSession && visitRate.add(ip) > highVolumeSessions {
		sig |= sigHighVolume.bit
	}
	return sig
}

// Data-centre and hosting networks. CDN networks (Akamai, Cloudflare, Fastly) are deliberately not listed:
// Apple's iCloud Private Relay and Cloudflare WARP send real people's traffic through them.
var dataCenterRE = regexp.MustCompile(`(?i)amazon|aws|google cloud|google llc|microsoft corporation|azure|digitalocean|` +
	`linode|akamai connected cloud|hetzner|ovh|contabo|vultr|choopa|constant company|oracle|alibaba|tencent|huawei cloud|` +
	`scaleway|online s\.a\.s|leaseweb|m247|datacamp|hostinger|ionos|1&1|g-core|colocrossing|psychz|quadranet|` +
	`servers\.com|hostwinds|kamatera|upcloud|netcup|hostroyale|zenlayer|ponynet|frantech|buyvm|racknerd|` +
	`dedipath|serverius|worldstream|nforce|hivelocity|limestone|packet host|equinix metal|fly\.io|heroku`)

func isDataCenter(network string) bool { return network != "" && dataCenterRE.MatchString(network) }

// ---- New-visit rate per IP (in memory only; IPs are never written to disk) ----

const (
	highVolumeWindow   = 10 * time.Minute
	highVolumeSessions = 20
)

type rateCounter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

var visitRate = newRateCounter()

func newRateCounter() *rateCounter {
	rc := &rateCounter{windows: map[string]*rateWindow{}}
	go func() {
		for range time.Tick(time.Minute) {
			rc.mu.Lock()
			for ip, w := range rc.windows {
				if time.Since(w.start) > highVolumeWindow {
					delete(rc.windows, ip)
				}
			}
			rc.mu.Unlock()
		}
	}()
	return rc
}

// add records a new visit from ip and returns how many it made in the current window.
func (rc *rateCounter) add(ip string) int {
	if ip == "" {
		return 0
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	w := rc.windows[ip]
	if w == nil || time.Since(w.start) > highVolumeWindow {
		w = &rateWindow{start: time.Now()}
		rc.windows[ip] = w
	}
	w.count++
	return w.count
}

// fastNavigation: 4+ page views averaging under 2 seconds each.
func fastNavigation(pageviews, startedAt, now int64) bool {
	return pageviews >= 4 && (now-startedAt)/pageviews < 2000
}

func init() {
	// Keep the label list and weights honest if someone adds a signal.
	seen := map[int64]bool{}
	for _, s := range botSignals {
		if seen[s.bit] || strings.TrimSpace(s.label) == "" {
			panic("bot signals must have unique bits and labels")
		}
		seen[s.bit] = true
	}
}
