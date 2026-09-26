package main

import (
	"net/url"
	"regexp"
	"strings"
)

// Small, dependency-free user-agent and referrer classification.

var botRE = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|headless|lighthouse|pagespeed|preview|facebookexternalhit|embedly|quora link|whatsapp|curl|wget|python-requests|axios|node-fetch|go-http-client`)

func isBot(ua string) bool { return ua == "" || botRE.MatchString(ua) }

type agent struct{ Browser, OS, Device string }

func parseUA(ua string, screenWidth float64) agent {
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(ua, x) {
				return true
			}
		}
		return false
	}
	a := agent{"Other", "Other", "Desktop"}
	switch {
	case has("Edg/"):
		a.Browser = "Edge"
	case has("OPR/", "Opera"):
		a.Browser = "Opera"
	case has("SamsungBrowser"):
		a.Browser = "Samsung Internet"
	case has("Firefox", "FxiOS"):
		a.Browser = "Firefox"
	case has("Chrome", "CriOS"):
		a.Browser = "Chrome"
	case has("Safari"):
		a.Browser = "Safari"
	}
	switch {
	case has("iPhone", "iPad", "iPod"):
		a.OS = "iOS"
	case has("Android"):
		a.OS = "Android"
	case has("Windows"):
		a.OS = "Windows"
	case has("CrOS"):
		a.OS = "ChromeOS"
	case has("Mac OS X", "Macintosh"):
		a.OS = "macOS"
	case has("Linux"):
		a.OS = "Linux"
	}
	switch {
	case has("iPad", "Tablet") || (has("Android") && !has("Mobile")):
		a.Device = "Tablet"
	case has("Mobi", "iPhone", "iPod"):
		a.Device = "Mobile"
	case screenWidth > 0 && screenWidth < 768:
		a.Device = "Mobile"
	}
	return a
}

var knownSources = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`(^|\.)google\.`), "Google"},
	{regexp.MustCompile(`(^|\.)bing\.com$`), "Bing"},
	{regexp.MustCompile(`(^|\.)duckduckgo\.com$`), "DuckDuckGo"},
	{regexp.MustCompile(`(^|\.)yahoo\.`), "Yahoo"},
	{regexp.MustCompile(`(^|\.)baidu\.com$`), "Baidu"},
	{regexp.MustCompile(`(^|\.)yandex\.`), "Yandex"},
	{regexp.MustCompile(`(^|\.)ecosia\.org$`), "Ecosia"},
	{regexp.MustCompile(`(^|\.)(facebook\.com|fb\.me|fb\.com)$`), "Facebook"},
	{regexp.MustCompile(`(^|\.)instagram\.com$`), "Instagram"},
	{regexp.MustCompile(`(^|\.)(twitter\.com|x\.com|t\.co)$`), "X (Twitter)"},
	{regexp.MustCompile(`(^|\.)(linkedin\.com|lnkd\.in)$`), "LinkedIn"},
	{regexp.MustCompile(`(^|\.)reddit\.com$`), "Reddit"},
	{regexp.MustCompile(`(^|\.)(youtube\.com|youtu\.be)$`), "YouTube"},
	{regexp.MustCompile(`(^|\.)tiktok\.com$`), "TikTok"},
	{regexp.MustCompile(`(^|\.)pinterest\.`), "Pinterest"},
	{regexp.MustCompile(`(^|\.)github\.com$`), "GitHub"},
	{regexp.MustCompile(`(^|\.)news\.ycombinator\.com$`), "Hacker News"},
	{regexp.MustCompile(`(^|\.)(chatgpt\.com|chat\.openai\.com)$`), "ChatGPT"},
	{regexp.MustCompile(`(^|\.)perplexity\.ai$`), "Perplexity"},
	{regexp.MustCompile(`(^|\.)claude\.ai$`), "Claude"},
}

// refHost is the hostname of a URL without "www.", or "" if missing/invalid.
func refHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// classifySource names a traffic source: utm_source wins, then a known referrer, then the referring host.
func classifySource(referrer, utmSource string) string {
	if utmSource != "" {
		return truncate(strings.TrimSpace(utmSource), 100)
	}
	host := refHost(referrer)
	if host == "" {
		return "Direct"
	}
	for _, k := range knownSources {
		if k.re.MatchString(host) {
			return k.name
		}
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Don't cut a multi-byte character in half.
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
