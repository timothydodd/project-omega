package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Session replay. On sites with replay switched on, the server picks which sessions to record and says so in
// the page view response. The tracker then loads /replay.js (rrweb's recorder plus tracker/replay.js), which
// uploads gzip-compressed NDJSON chunks of rrweb events. Chunks are stored exactly as uploaded. To play a recording
// they are decompressed in order and re-compressed as one gzip stream: browsers stop reading a response at the end
// of its first gzip member, so the stored chunks can't simply be sent one after another.
//
// A recording is one page load (one tab), so events from two tabs never mix. A session can have several.

const (
	maxReplayChunk     = 1 << 20       // compressed bytes per upload
	maxReplayExpanded  = 10 << 20      // decompressed bytes per upload
	maxRecordingBytes  = 30 << 20      // compressed bytes per recording
	maxRecordingLength = 4 * time.Hour // recordings stop after this long
	maxReplaySeq       = 100_000       // chunk numbers a recording may use
	replayClockSkew    = int64(time.Hour / time.Millisecond)
	defaultRetention   = 7    // days
	defaultReplayMaxMB = 1024 // total replay storage before the oldest recordings go
)

// sampled picks the same sessions every time, so a session is either recorded on every page or on none.
func sampled(sessionID string, percent int64) bool {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	return int64(h.Sum32()%100) < percent
}

// shouldRecord: replay is on for the site, the session isn't a likely bot, it's in the sample, and on
// "cookies after consent" sites the visitor has accepted (the session runs on the cookie).
func shouldRecord(site Site, id identity, bot bool) bool {
	if !site.Replay || bot {
		return false
	}
	if site.Privacy == "consent" && id.method != "cookie" {
		return false
	}
	return sampled(id.sessionID, site.ReplaySample)
}

// decideReplay stores whether a session is recorded and returns it.
func decideReplay(q querier, site Site, id identity, bot bool) (bool, error) {
	record := shouldRecord(site, id, bot)
	_, err := q.Exec("UPDATE sessions SET replay = ? WHERE id = ?", record, id.sessionID)
	return record, err
}

// ---- Upload ----

type replayUpload struct {
	key, pageviewID, recordingID string
	seq                          int64
	body                         []byte
}

var errStopRecording = errors.New("stop recording")

func handleReplayUpload(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReplayChunk))
	if err != nil {
		fail(w, 413, "Payload too large")
		return
	}
	q := r.URL.Query()
	seq, err := strconv.ParseInt(q.Get("n"), 10, 64)
	if err != nil {
		fail(w, 400, "Invalid n")
		return
	}
	up := replayUpload{key: q.Get("k"), pageviewID: q.Get("p"), recordingID: q.Get("r"), seq: seq, body: body}
	switch e := storeReplayChunk(r, up, time.Now()); {
	case e == nil:
		w.WriteHeader(202)
	case e.status == 200:
		writeJSON(w, 200, map[string]int{"stop": 1})
	default:
		if e.status == 500 {
			log.Printf("replay: %s", e.msg)
			e.msg = "Server error"
		}
		fail(w, e.status, e.msg)
	}
}

// storeReplayChunk checks an upload and saves it. A 200 error means "stop recording" (not recorded, or too long).
func storeReplayChunk(r *http.Request, up replayUpload, now time.Time) *collectError {
	site, ok := siteByKey(up.key)
	if !ok {
		return &collectError{404, "Unknown site key"}
	}
	if !idRE.MatchString(up.pageviewID) || !idRE.MatchString(up.recordingID) {
		return &collectError{400, "Invalid ids"}
	}
	if up.seq < 0 || up.seq > maxReplaySeq {
		return &collectError{400, "Invalid n"}
	}
	if isBot(r.UserAgent()) {
		return &collectError{200, "stop"}
	}
	if origin := refHost(r.Header.Get("Origin")); origin != "" && !hostAllowed(site, origin) {
		return &collectError{403, "Domain not allowed for this site"}
	}
	id := existingIdentity(site, &Hit{P: up.pageviewID})
	if id == nil {
		return &collectError{409, "Send a pageview first"}
	}
	s, _ := rowOf(db, "SELECT replay, bot FROM sessions WHERE id = ? AND site_id = ?", id.sessionID, site.ID)
	if !site.Replay || s == nil || i64(s, "replay") != 1 || i64(s, "bot") == 1 {
		return &collectError{200, "stop"}
	}

	ms := now.UnixMilli()
	first, last, err := replayTimes(up.body)
	if err != nil {
		return &collectError{400, err.Error()}
	}
	// Timestamps come from the visitor's clock: keep them near ours so retention can't be dodged.
	first = min(max(first, ms-replayClockSkew), ms+replayClockSkew)
	last = min(max(last, first), ms+replayClockSkew)

	rec, err := rowOf(db, `SELECT min(session_id) session_id, min(ts_first) started, coalesce(sum(bytes), 0) bytes
		FROM replay_chunks WHERE recording_id = ?`, up.recordingID)
	if err != nil {
		return &collectError{500, err.Error()}
	}
	if owner := str(rec, "session_id"); owner != "" && owner != id.sessionID {
		return &collectError{409, "Recording belongs to another session"}
	}
	if started := i64(rec, "started"); started > 0 && ms-started > maxRecordingLength.Milliseconds() {
		return &collectError{200, "stop"}
	}
	if i64(rec, "bytes")+int64(len(up.body)) > maxRecordingBytes {
		return &collectError{200, "stop"}
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO replay_chunks (site_id, session_id, recording_id, seq, ts_first, ts_last, bytes, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, site.ID, id.sessionID, up.recordingID, up.seq, first, last, len(up.body), up.body); err != nil {
		return &collectError{500, err.Error()}
	}
	return nil
}

// replayTimes checks a chunk is gzip-compressed NDJSON of rrweb events and returns its first and last timestamps.
func replayTimes(chunk []byte) (first, last int64, err error) {
	zr, err := gzip.NewReader(bytes.NewReader(chunk))
	if err != nil {
		return 0, 0, errors.New("Body must be gzip")
	}
	limited := &io.LimitedReader{R: zr, N: maxReplayExpanded + 1}
	dec := json.NewDecoder(limited)
	n := 0
	for {
		var ev struct {
			Type      *int  `json:"type"`
			Timestamp int64 `json:"timestamp"`
		}
		if err := dec.Decode(&ev); err == io.EOF {
			break
		} else if err != nil {
			if limited.N <= 0 {
				return 0, 0, errors.New("Chunk too large")
			}
			return 0, 0, errors.New("Invalid events")
		}
		if ev.Type == nil || ev.Timestamp <= 0 {
			return 0, 0, errors.New("Invalid events")
		}
		if n == 0 {
			first = ev.Timestamp
		}
		last = max(last, ev.Timestamp)
		n++
	}
	if n == 0 {
		return 0, 0, errors.New("No events")
	}
	return first, last, nil
}

// ---- Dashboard ----

func listRecordings(sc siteScope, sessionID string) ([]Row, error) {
	w, args := sc.where("site_id")
	return rowsOf(db, `SELECT recording_id id, min(ts_first) started_at, max(ts_last) ended_at, sum(bytes) bytes
		FROM replay_chunks WHERE `+w+` AND session_id = ? GROUP BY recording_id ORDER BY started_at`, append(args, sessionID)...)
}

// serveRecording streams a recording as NDJSON events, gzip-compressed as a single gzip member.
func serveRecording(w http.ResponseWriter, r *http.Request, sc siteScope, sessionID, recordingID string) {
	where, args := sc.where("site_id")
	rows, err := db.Query("SELECT data FROM replay_chunks WHERE "+where+" AND session_id = ? AND recording_id = ? ORDER BY seq",
		append(args, sessionID, recordingID)...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "private, no-cache")
	var out io.Writer = w
	var gz *gzip.Writer
	wrote := false
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			break
		}
		if !wrote {
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				h.Set("Content-Encoding", "gzip")
				gz, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
				out = gz
			}
			w.WriteHeader(200)
			wrote = true
		}
		if zr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
			io.Copy(out, zr)
		}
	}
	if gz != nil {
		gz.Close()
	}
	if !wrote {
		fail(w, 404, "Recording not found")
	}
}

// ---- Retention ----

func replayRetentionDays() (days int64, fromEnv bool) {
	if v, err := strconv.ParseInt(os.Getenv("OMEGA_REPLAY_RETENTION_DAYS"), 10, 64); err == nil && v > 0 {
		return v, true
	}
	if v, err := strconv.ParseInt(getSetting("replay_retention_days"), 10, 64); err == nil && v > 0 {
		return v, false
	}
	return defaultRetention, false
}

func replayMaxBytes() int64 {
	mb, err := strconv.ParseInt(os.Getenv("OMEGA_REPLAY_MAX_MB"), 10, 64)
	if err != nil || mb <= 0 {
		mb = defaultReplayMaxMB
	}
	return mb << 20
}

func replayBytes() int64 {
	r, _ := rowOf(db, "SELECT coalesce(sum(bytes), 0) n FROM replay_chunks")
	return i64(r, "n")
}

// purgeReplays deletes recordings past the retention period, recordings of sessions now flagged as bots, and
// then the oldest recordings while the total is over the storage cap. Analytics data is never touched.
func purgeReplays(now time.Time, days, maxBytes int64) error {
	cutoff := now.UnixMilli() - days*dayMs
	if _, err := db.Exec(`DELETE FROM replay_chunks WHERE recording_id IN
		(SELECT recording_id FROM replay_chunks GROUP BY recording_id HAVING max(ts_last) < ?)`, cutoff); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM replay_chunks WHERE EXISTS
		(SELECT 1 FROM sessions s WHERE s.id = replay_chunks.session_id AND s.bot = 1)`); err != nil {
		return err
	}
	over := replayBytes() - maxBytes
	if over <= 0 {
		return nil
	}
	oldest, err := rowsOf(db, `SELECT recording_id, sum(bytes) bytes FROM replay_chunks
		GROUP BY recording_id ORDER BY max(ts_last)`)
	if err != nil {
		return err
	}
	for _, rec := range oldest {
		if over <= 0 {
			break
		}
		if _, err := db.Exec("DELETE FROM replay_chunks WHERE recording_id = ?", rec["recording_id"]); err != nil {
			return err
		}
		over -= i64(rec, "bytes")
	}
	return nil
}

func runReplayRetention() {
	for {
		days, _ := replayRetentionDays()
		if err := purgeReplays(time.Now(), days, replayMaxBytes()); err != nil {
			log.Printf("replay retention: %v", err)
		}
		time.Sleep(time.Hour)
	}
}
