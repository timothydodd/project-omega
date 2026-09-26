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
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Country lookup uses DB-IP's free "IP to Country Lite" database (CC BY 4.0, updated monthly):
// https://db-ip.com/db/download/ip-to-country-lite. The visitor's IP is only used for the lookup, never stored.

var geo *maxminddb.Reader

// geoipPath is OMEGA_GEOIP_DB, or dbip-country-lite.mmdb next to the database file.
func geoipPath(dbPath string) string {
	return env("OMEGA_GEOIP_DB", filepath.Join(filepath.Dir(dbPath), "dbip-country-lite.mmdb"))
}

func openGeoIP(path string) {
	r, err := maxminddb.Open(path)
	if err != nil {
		log.Printf("Country lookup off: no database at %s (run `omega geoip-update` to download it)", path)
		return
	}
	geo = r
	log.Printf("Country lookup on: %s (built %s)", path, time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC().Format("2006-01-02"))
}

// lookupCountry returns the ISO country code for an IP, or "".
func lookupCountry(ip string) string {
	if geo == nil {
		return ""
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := geo.Lookup(addr.Unmap()).Decode(&rec); err != nil {
		return ""
	}
	return rec.Country.ISOCode
}

// updateGeoIP downloads this month's DB-IP country database (or last month's, early in the month).
func updateGeoIP(path string) error {
	now := time.Now().UTC()
	var lastErr error
	for _, month := range []time.Time{now, now.AddDate(0, -1, 0)} {
		url := fmt.Sprintf("https://download.db-ip.com/free/dbip-country-lite-%s.mmdb.gz", month.Format("2006-01"))
		if lastErr = download(url, path); lastErr == nil {
			fmt.Printf("Saved %s\nfrom %s\nIP Geolocation by DB-IP (https://db-ip.com), CC BY 4.0.\n", path, url)
			return nil
		}
	}
	return lastErr
}

func download(url, path string) error {
	resp, err := http.Get(url)
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
	// Write to a temp file and rename, so a running server never sees a half-written database.
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
