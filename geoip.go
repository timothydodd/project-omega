package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// IP lookups use DB-IP's free "Lite" databases (CC BY 4.0, published monthly):
//   - IP to Country: the visitor's country
//   - IP to ASN:     the network that owns the IP, used to spot data-centre (bot) traffic
//
// https://db-ip.com/db/lite.php. The visitor's IP is only used for the lookups, never stored.
//
// Where the files come from, newest wins:
//   - downloads in the data folder (next to the SQLite file), named e.g. dbip-country-lite-2026-10.mmdb
//   - a bundled copy set by OMEGA_GEOIP_DB / OMEGA_ASN_DB (the container image ships one)
//
// The server downloads each new month's release by itself and swaps it in without a restart.
// Set OMEGA_IP_DB_UPDATE=off to disable that (e.g. no outbound internet).

type ipDatabase struct {
	name, envVar, prefix, urlPrefix string
	reader                          atomic.Pointer[maxminddb.Reader]
}

var (
	countryDB = &ipDatabase{name: "Country lookup", envVar: "OMEGA_GEOIP_DB", prefix: "dbip-country-lite",
		urlPrefix: "https://download.db-ip.com/free/dbip-country-lite-"}
	networkDB = &ipDatabase{name: "Network lookup", envVar: "OMEGA_ASN_DB", prefix: "dbip-asn-lite",
		urlPrefix: "https://download.db-ip.com/free/dbip-asn-lite-"}
	ipDatabases = []*ipDatabase{countryDB, networkDB}
)

// candidates lists this database's files, newest first by month in the name (bundled copy last).
func (d *ipDatabase) candidates(dataDir string) []string {
	found, _ := filepath.Glob(filepath.Join(dataDir, d.prefix+"-*.mmdb"))
	sort.Sort(sort.Reverse(sort.StringSlice(found)))
	// Files from older versions without a month in the name.
	if legacy := filepath.Join(dataDir, d.prefix+".mmdb"); fileExists(legacy) {
		found = append(found, legacy)
	}
	if bundled := os.Getenv(d.envVar); bundled != "" {
		found = append(found, bundled)
	}
	return found
}

// load opens the newest usable file and swaps it in.
func (d *ipDatabase) load(dataDir string) bool {
	var best *maxminddb.Reader
	var bestPath string
	for _, path := range d.candidates(dataDir) {
		r, err := maxminddb.Open(path)
		if err != nil {
			continue
		}
		if best == nil || r.Metadata.BuildEpoch > best.Metadata.BuildEpoch {
			if best != nil {
				best.Close()
			}
			best, bestPath = r, path
		} else {
			r.Close()
		}
	}
	if best == nil {
		return false
	}
	if old := d.reader.Swap(best); old != nil {
		// Let in-flight lookups finish before unmapping the old file.
		time.AfterFunc(time.Minute, func() { old.Close() })
	}
	log.Printf("%s on: %s (built %s)", d.name, bestPath, d.built().Format("2006-01-02"))
	return true
}

func (d *ipDatabase) built() time.Time {
	if r := d.reader.Load(); r != nil {
		return time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC()
	}
	return time.Time{}
}

func (d *ipDatabase) ready() bool { return d.reader.Load() != nil }

func (d *ipDatabase) lookup(ip string, rec any) {
	r := d.reader.Load()
	if r == nil {
		return
	}
	if addr, err := netip.ParseAddr(ip); err == nil {
		r.Lookup(addr.Unmap()).Decode(rec)
	}
}

// updateMu keeps the background updater and the Settings button from downloading at the same time.
var updateMu sync.Mutex

// update downloads the current month's release into dataDir if what's loaded is older, then swaps it in.
func (d *ipDatabase) update(dataDir string) error {
	updateMu.Lock()
	defer updateMu.Unlock()
	now := time.Now().UTC()
	month := now.Format("2006-01")
	if d.built().Format("2006-01") >= month {
		return nil // already current
	}
	path := filepath.Join(dataDir, d.prefix+"-"+month+".mmdb")
	if err := download(d.urlPrefix+month+".mmdb.gz", path); err != nil {
		// Early in the month the new release may not be out yet; fall back to last month's.
		prev := now.AddDate(0, -1, 0).Format("2006-01")
		if d.ready() && d.built().Format("2006-01") >= prev {
			return nil
		}
		path = filepath.Join(dataDir, d.prefix+"-"+prev+".mmdb")
		if err := download(d.urlPrefix+prev+".mmdb.gz", path); err != nil {
			return err
		}
	}
	if !d.load(dataDir) {
		return fmt.Errorf("could not open %s", path)
	}
	d.prune(dataDir)
	return nil
}

// prune deletes older downloads, keeping the newest two (the one in use and one fallback).
func (d *ipDatabase) prune(dataDir string) {
	found, _ := filepath.Glob(filepath.Join(dataDir, d.prefix+"-*.mmdb"))
	sort.Sort(sort.Reverse(sort.StringSlice(found)))
	for i, f := range found {
		if i >= 2 {
			os.Remove(f) // on Windows this fails while the file is still mapped; it's retried next time
		}
	}
}

func openIPDatabases(dbPath string) {
	dataDir := filepath.Dir(dbPath)
	for _, d := range ipDatabases {
		if !d.load(dataDir) {
			log.Printf("%s off: no database yet (it downloads automatically, or run `omega geoip-update`)", d.name)
		}
	}
}

// autoUpdateIPDatabases checks twice a day for a new monthly release.
func autoUpdateIPDatabases(dbPath string) {
	if strings.EqualFold(os.Getenv("OMEGA_IP_DB_UPDATE"), "off") {
		return
	}
	dataDir := filepath.Dir(dbPath)
	for {
		for _, d := range ipDatabases {
			if err := d.update(dataDir); err != nil {
				log.Printf("%s update failed (will retry): %v", d.name, err)
			}
		}
		time.Sleep(12 * time.Hour)
	}
}

type ipUpdateResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "updated", "current" or "failed"
	Built  string `json:"built,omitempty"`
	Error  string `json:"error,omitempty"`
}

// updateIPDatabasesNow is the Settings page's "Check for updates" button.
func updateIPDatabasesNow(dataDir string) []ipUpdateResult {
	out := []ipUpdateResult{}
	for _, d := range ipDatabases {
		before := d.built()
		res := ipUpdateResult{Name: d.name, Status: "current"}
		if err := d.update(dataDir); err != nil {
			res.Status, res.Error = "failed", err.Error()
			log.Printf("%s update failed: %v", d.name, err)
		} else if d.built().After(before) {
			res.Status = "updated"
		}
		if d.ready() {
			res.Built = d.built().Format("2006-01-02")
		}
		out = append(out, res)
	}
	return out
}

// updateIPDatabases is the `omega geoip-update` command.
func updateIPDatabases(dbPath string) error {
	dataDir := filepath.Dir(dbPath)
	for _, d := range ipDatabases {
		d.load(dataDir)
		if err := d.update(dataDir); err != nil {
			return fmt.Errorf("%s: %w", d.name, err)
		}
		fmt.Printf("%s: built %s\n", d.name, d.built().Format("2006-01-02"))
	}
	fmt.Println("IP Geolocation by DB-IP (https://db-ip.com), CC BY 4.0.")
	return nil
}

// lookupCountry returns the ISO country code for an IP, or "".
func lookupCountry(ip string) string {
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	countryDB.lookup(ip, &rec)
	return rec.Country.ISOCode
}

// lookupNetwork returns the organisation that owns the IP's network (e.g. "Comcast Cable", "Amazon.com, Inc."), or "".
func lookupNetwork(ip string) string {
	var rec struct {
		Org string `maxminddb:"autonomous_system_organization"`
	}
	networkDB.lookup(ip, &rec)
	return rec.Org
}

func download(url, path string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename, so a half-written file is never picked up.
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, gz); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	check, err := maxminddb.Open(tmp)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("downloaded file is not a valid database: %w", err)
	}
	check.Close()
	return os.Rename(tmp, path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
