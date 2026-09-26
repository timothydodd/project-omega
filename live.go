package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// A visitor counts as live if we heard from their tab within this window. The tracker pings every 15s.
const liveWindow = 60 * time.Second

type LiveVisitor struct {
	SessionID     string  `json:"sessionId"`
	VisitorID     string  `json:"visitorId"`
	UserID        *string `json:"userId"`
	UserName      *string `json:"userName"` // traits.name or traits.email from identify()
	Path          string  `json:"path"`
	Title         *string `json:"title"`
	Source        string  `json:"source"`
	Device        string  `json:"device"`
	Browser       string  `json:"browser"`
	Country       *string `json:"country"`
	StartedAt     int64   `json:"startedAt"`
	PageStartedAt int64   `json:"pageStartedAt"`
	LastSeen      int64   `json:"lastSeen"`
	Pageviews     int64   `json:"pageviews"`
}

type LiveSnapshot struct {
	Now      int64         `json:"now"`
	Count    int           `json:"count"`
	Visitors []LiveVisitor `json:"visitors"`
}

var live = &presence{
	bySite: map[int64]map[string]*LiveVisitor{},
	subs:   map[int64]map[chan []byte]struct{}{},
	dirty:  map[int64]bool{},
}

type presence struct {
	mu     sync.Mutex
	bySite map[int64]map[string]*LiveVisitor
	subs   map[int64]map[chan []byte]struct{}
	dirty  map[int64]bool
}

func (p *presence) site(id int64) map[string]*LiveVisitor {
	m := p.bySite[id]
	if m == nil {
		m = map[string]*LiveVisitor{}
		p.bySite[id] = m
	}
	return m
}

func (p *presence) touch(siteID int64, v LiveVisitor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.site(siteID)
	if prev := m[v.SessionID]; prev != nil && prev.Path == v.Path {
		v.PageStartedAt = prev.PageStartedAt
	}
	m[v.SessionID] = &v
	p.dirty[siteID] = true
}

// ping refreshes a visitor; false means they're not on the live list (restart, or a tab coming back).
func (p *presence) ping(siteID int64, sessionID string, now int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.bySite[siteID][sessionID]; v != nil {
		v.LastSeen = now
		return true
	}
	return false
}

func (p *presence) leave(siteID int64, sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.bySite[siteID][sessionID]; ok {
		delete(p.bySite[siteID], sessionID)
		p.dirty[siteID] = true
	}
}

func (p *presence) identify(siteID int64, sessionID, userID string, userName *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.bySite[siteID][sessionID]; v != nil {
		v.UserID = &userID
		if userName != nil {
			v.UserName = userName
		}
		p.dirty[siteID] = true
	}
}

// rekey moves a session to new ids (a cookieless session adopted by a cookie after consent).
func (p *presence) rekey(siteID int64, oldID, newID, visitorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.bySite[siteID]
	if v := m[oldID]; v != nil {
		delete(m, oldID)
		v.SessionID, v.VisitorID = newID, visitorID
		m[newID] = v
		p.dirty[siteID] = true
	}
}

func (p *presence) snapshot(siteID int64) LiveSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked(siteID)
}

func (p *presence) snapshotLocked(siteID int64) LiveSnapshot {
	now := time.Now().UnixMilli()
	out := []LiveVisitor{}
	for _, v := range p.bySite[siteID] {
		if now-v.LastSeen < liveWindow.Milliseconds() {
			out = append(out, *v)
		}
	}
	slices.SortFunc(out, func(a, b LiveVisitor) int { return cmp.Compare(b.LastSeen, a.LastSeen) })
	return LiveSnapshot{Now: now, Count: len(out), Visitors: out}
}

// serveStream is the Server-Sent Events endpoint the dashboard listens on.
func (p *presence) serveStream(w http.ResponseWriter, r *http.Request, siteID int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 4)
	p.mu.Lock()
	if p.subs[siteID] == nil {
		p.subs[siteID] = map[chan []byte]struct{}{}
	}
	p.subs[siteID][ch] = struct{}{}
	first, _ := json.Marshal(p.snapshotLocked(siteID))
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.subs[siteID], ch)
		p.mu.Unlock()
	}()

	fmt.Fprintf(w, "data: %s\n\n", first)
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// run expires stale visitors and pushes updates to open dashboards.
func (p *presence) run() {
	tick := time.NewTicker(2 * time.Second)
	for now := range tick.C {
		p.mu.Lock()
		ms := now.UnixMilli()
		for siteID, m := range p.bySite {
			for id, v := range m {
				if ms-v.LastSeen >= liveWindow.Milliseconds() {
					delete(m, id)
					p.dirty[siteID] = true
				}
			}
		}
		// Refresh at least every 10s so "time on page" counters stay honest.
		periodic := ms%10_000 < 2_000
		for siteID, subs := range p.subs {
			if len(subs) == 0 || (!p.dirty[siteID] && !periodic) {
				continue
			}
			msg, _ := json.Marshal(p.snapshotLocked(siteID))
			for ch := range subs {
				select {
				case ch <- msg:
				default: // slow client: skip this update
				}
			}
		}
		clear(p.dirty)
		p.mu.Unlock()
	}
}

// restore rebuilds presence after a restart from sessions that were active a moment ago.
func (p *presence) restore() {
	rows, err := rowsOf(db, `
		SELECT s.*, v.user_id, v.traits FROM sessions s
		LEFT JOIN visitors v ON v.site_id = s.site_id AND v.id = s.visitor_id
		WHERE s.last_seen >= ?`, time.Now().Add(-liveWindow).UnixMilli())
	if err != nil {
		return
	}
	for _, r := range rows {
		v := liveFromSession(r, i64(r, "last_seen"))
		p.touch(i64(r, "site_id"), v)
	}
}

// liveFromSession builds a live entry from a sessions row joined with visitors (user_id, traits).
func liveFromSession(r Row, pageStartedAt int64) LiveVisitor {
	return LiveVisitor{
		SessionID: str(r, "id"), VisitorID: str(r, "visitor_id"),
		UserID: strPtr(str(r, "user_id")), UserName: nameFromTraits(str(r, "traits")),
		Path: str(r, "exit_path"), Title: strPtr(str(r, "exit_title")),
		Source: str(r, "source"), Device: str(r, "device"), Browser: str(r, "browser"), Country: strPtr(str(r, "country")),
		StartedAt: i64(r, "started_at"), PageStartedAt: pageStartedAt, LastSeen: time.Now().UnixMilli(), Pageviews: i64(r, "pageviews"),
	}
}

func nameFromTraits(traits string) *string {
	if traits == "" {
		return nil
	}
	var t map[string]any
	if json.Unmarshal([]byte(traits), &t) != nil {
		return nil
	}
	for _, k := range []string{"name", "email"} {
		if s, ok := t[k].(string); ok && s != "" {
			return &s
		}
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
