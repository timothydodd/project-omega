package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"

func openTestDB(t *testing.T) {
	t.Helper()
	if err := openDB(filepath.Join(t.TempDir(), "omega.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}

func gzipLines(t *testing.T, lines ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(strings.Join(lines, "\n") + "\n"))
	w.Close()
	return b.Bytes()
}

func rrEvent(ts int64) string {
	return fmt.Sprintf(`{"type":3,"data":{"source":1},"timestamp":%d}`, ts)
}

func TestShouldRecord(t *testing.T) {
	site := Site{Replay: true, ReplaySample: 100, Privacy: "cookies"}
	cookie := identity{"v", "session-a", "cookie"}
	if !shouldRecord(site, cookie, false) {
		t.Error("100% sample should record")
	}
	if shouldRecord(site, cookie, true) {
		t.Error("bots are never recorded")
	}
	if shouldRecord(Site{Replay: false, ReplaySample: 100}, cookie, false) {
		t.Error("replay off should not record")
	}
	consent := Site{Replay: true, ReplaySample: 100, Privacy: "consent"}
	if shouldRecord(consent, identity{"v", "s", "hash"}, false) || !shouldRecord(consent, identity{"v", "s", "cookie"}, false) {
		t.Error("consent sites record only once the session runs on the cookie")
	}
	// Sampling is sticky per session and roughly matches the rate.
	in := 0
	for i := range 2000 {
		id := fmt.Sprintf("session-%d", i)
		if sampled(id, 25) != sampled(id, 25) {
			t.Fatal("sampling must be deterministic")
		}
		if sampled(id, 25) {
			in++
		}
	}
	if in < 400 || in > 600 {
		t.Errorf("25%% sample picked %d of 2000", in)
	}
}

// A page view on a replay site answers "record", and the tracker's cookie session gets the flag.
func TestCollectAnswersReplay(t *testing.T) {
	openTestDB(t)
	site, _ := createSite(siteInput{Name: "A", Domains: "a.test", Privacy: "cookies", Replay: true, ReplaySample: 100})
	hit := func(t_, sessionID string) []byte {
		b, _ := json.Marshal(map[string]any{"k": site.Key, "t": t_, "v": "visitor-aaaa", "s": sessionID, "p": "pageview-" + sessionID,
			"u": "https://a.test/pricing", "sw": 1920, "sh": 1080})
		return b
	}
	r := httptest.NewRequest("POST", "/api/collect", nil)
	r.Header.Set("User-Agent", chromeUA) // headers a real browser sends, so the visit doesn't look like a bot
	r.Header.Set("Accept-Language", "en-US")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	record, e := collect(r, hit("pageview", "session-aaaa"))
	if e != nil || !record {
		t.Fatalf("pageview on a replay site: record=%v err=%+v", record, e)
	}
	updateSite(site.ID, siteInput{Name: "A", Domains: "a.test", Privacy: "cookies"})
	if record, _ := collect(r, hit("pageview", "session-bbbb")); record {
		t.Error("replay off: pageview should not ask to record")
	}
}

func TestReplayUploadAndServe(t *testing.T) {
	openTestDB(t)
	site, _ := createSite(siteInput{Name: "A", Domains: "a.test", Privacy: "cookies", Replay: true, ReplaySample: 100})
	other, _ := createSite(siteInput{Name: "B", Domains: "b.test", Privacy: "cookies", Replay: true, ReplaySample: 100})
	now := time.Now()
	ms := now.UnixMilli()
	for _, s := range []struct {
		id     string
		replay int
	}{{"session-rec", 1}, {"session-off", 0}} {
		db.Exec(`INSERT INTO sessions (id, site_id, visitor_id, started_at, last_seen, entry_path, exit_path, pageviews, source, replay)
			VALUES (?, ?, 'visitor-1', ?, ?, '/', '/', 1, 'Direct', ?)`, s.id, site.ID, ms, ms, s.replay)
		db.Exec("INSERT INTO pageviews (id, site_id, session_id, visitor_id, ts, path) VALUES (?, ?, ?, 'visitor-1', ?, '/')",
			"pv-"+s.id, site.ID, s.id, ms)
	}
	r := httptest.NewRequest("POST", "/api/replay", nil)
	r.Header.Set("User-Agent", chromeUA)
	up := func(key, pv, rec string, seq int64, body []byte) *collectError {
		return storeReplayChunk(r, replayUpload{key: key, pageviewID: pv, recordingID: rec, seq: seq, body: body}, now)
	}

	if e := up(site.Key, "pv-session-rec", "recording-1", 0, gzipLines(t, rrEvent(ms-2000), rrEvent(ms-1000))); e != nil {
		t.Fatalf("first chunk: %+v", e)
	}
	if e := up(site.Key, "pv-session-rec", "recording-1", 1, gzipLines(t, rrEvent(ms))); e != nil {
		t.Fatalf("second chunk: %+v", e)
	}
	for name, c := range map[string]struct {
		e    *collectError
		want int
	}{
		"not recorded":     {up(site.Key, "pv-session-off", "recording-2", 0, gzipLines(t, rrEvent(ms))), 200},
		"other site's key": {up(other.Key, "pv-session-rec", "recording-3", 0, gzipLines(t, rrEvent(ms))), 409},
		"not gzip":         {up(site.Key, "pv-session-rec", "recording-4", 0, []byte(rrEvent(ms))), 400},
		"not events":       {up(site.Key, "pv-session-rec", "recording-5", 0, gzipLines(t, `{"hello":1}`)), 400},
		"unknown key":      {up("site_nope", "pv-session-rec", "recording-6", 0, gzipLines(t, rrEvent(ms))), 404},
		"recording hijack": {storeReplayChunk(r, replayUpload{key: site.Key, pageviewID: "pv-session-off", recordingID: "recording-1", seq: 5, body: gzipLines(t, rrEvent(ms))}, now), 200},
		"bad sequence":     {up(site.Key, "pv-session-rec", "recording-7", -1, gzipLines(t, rrEvent(ms))), 400},
	} {
		if c.e == nil || c.e.status != c.want {
			t.Errorf("%s: got %+v, want status %d", name, c.e, c.want)
		}
	}

	recs, err := listRecordings(siteScope{ids: []int64{site.ID}}, "session-rec")
	if err != nil || len(recs) != 1 || i64(recs[0], "started_at") != ms-2000 || i64(recs[0], "ended_at") != ms {
		t.Fatalf("recordings: %v, %v", recs, err)
	}
	if recs, _ := listRecordings(siteScope{ids: []int64{other.ID}}, "session-rec"); len(recs) != 0 {
		t.Error("another site's scope must not see the recording")
	}

	// The stored chunks come back as all the events, in order, in a single gzip member (browsers ignore any after the first).
	for _, gz := range []bool{true, false} {
		w := httptest.NewRecorder()
		get := httptest.NewRequest("GET", "/api/sessions/session-rec/replays/recording-1", nil)
		if gz {
			get.Header.Set("Accept-Encoding", "gzip")
		}
		serveRecording(w, get, siteScope{ids: []int64{site.ID}}, "session-rec", "recording-1")
		body := io.Reader(w.Body)
		if gz {
			if w.Header().Get("Content-Encoding") != "gzip" {
				t.Fatal("expected gzip response")
			}
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			zr.Multistream(false)
			body = zr
		}
		got, _ := io.ReadAll(body)
		if want := strings.Join([]string{rrEvent(ms - 2000), rrEvent(ms - 1000), rrEvent(ms)}, "\n") + "\n"; string(got) != want {
			t.Errorf("gzip=%v: served\n%s\nwant\n%s", gz, got, want)
		}
	}

	// A session flagged as a bot loses its recording.
	db.Exec("UPDATE sessions SET bot_signals = ? WHERE id = 'session-rec'", sigDataCenter.bit|sigNoInteraction.bit)
	if bot, err := rescore(db, "session-rec", ms); err != nil || !bot {
		t.Fatalf("rescore: bot=%v err=%v", bot, err)
	}
	if recs, _ := listRecordings(siteScope{ids: []int64{site.ID}}, "session-rec"); len(recs) != 0 {
		t.Error("bot session should have no recordings")
	}
}

func TestPurgeReplays(t *testing.T) {
	openTestDB(t)
	site, _ := createSite(siteInput{Name: "A", Privacy: "cookies", Replay: true, ReplaySample: 100})
	now := time.Now()
	ms := now.UnixMilli()
	add := func(session, rec string, ts int64, size int) {
		db.Exec(`INSERT OR IGNORE INTO sessions (id, site_id, visitor_id, started_at, last_seen, entry_path, exit_path, source)
			VALUES (?, ?, 'v', ?, ?, '/', '/', 'Direct')`, session, site.ID, ts, ts)
		db.Exec(`INSERT INTO replay_chunks (site_id, session_id, recording_id, seq, ts_first, ts_last, bytes, data)
			VALUES (?, ?, ?, 0, ?, ?, ?, x'00')`, site.ID, session, rec, ts, ts, size)
	}
	add("s-old", "r-old", ms-10*dayMs, 10)
	add("s-bot", "r-bot", ms-dayMs, 10)
	add("s-a", "r-a", ms-3*dayMs, 600)
	add("s-b", "r-b", ms-2*dayMs, 600)
	add("s-c", "r-c", ms-dayMs, 600)
	db.Exec("UPDATE sessions SET bot = 1 WHERE id = 's-bot'")

	if err := purgeReplays(now, 7, 1300); err != nil {
		t.Fatal(err)
	}
	rows, _ := rowsOf(db, "SELECT recording_id FROM replay_chunks ORDER BY recording_id")
	var left []string
	for _, r := range rows {
		left = append(left, str(r, "recording_id"))
	}
	// Over 7 days old: gone. Bot: gone. Over the 1300-byte cap: the oldest (r-a) goes first.
	if strings.Join(left, ",") != "r-b,r-c" {
		t.Errorf("left %v, want [r-b r-c]", left)
	}
	if n, _ := rowOf(db, "SELECT count(*) n FROM sessions"); i64(n, "n") != 5 {
		t.Error("purging recordings must not delete sessions")
	}
}

func TestReplayScript(t *testing.T) {
	src, err := assets.ReadFile("tracker/replay.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := minifyJS(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"rrwebRecord", "__omegaReplay", "CompressionStream", "data-omega-mask", "data-omega-block"} {
		if !bytes.Contains(out, []byte(s)) {
			t.Errorf("minified replay.js lost %q", s)
		}
	}
	body := loadReplayScript().plain
	if !bytes.Contains(body, []byte("define,exports,module")) || !bytes.Contains(body, []byte(`g["rrwebRecord"]`)) {
		t.Error("replay.js should wrap rrweb's recorder")
	}
}
