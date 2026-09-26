package main

import (
	"slices"
	"testing"
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
