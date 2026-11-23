package server

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A device that adds the board to a Home Screen or installs it on a desktop
// fetches the icons and the manifest with no session and no identity. A
// file it cannot fetch leaves it nothing to draw, and it draws a letter.
func TestInstallFilesNeedNoIdentity(t *testing.T) {
	r := newRig(t)
	sizes := map[string]string{"/apple-touch-icon.png": "180x180", "/icon-192.png": "192x192", "/icon-512.png": "512x512"}
	for path, size := range sizes {
		resp, body := r.page(path, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if len(body) < 24 || !strings.HasPrefix(body, "\x89PNG") {
			t.Fatalf("%s is not a PNG", path)
		}
		b := []byte(body)
		if got := fmt.Sprintf("%dx%d", binary.BigEndian.Uint32(b[16:20]), binary.BigEndian.Uint32(b[20:24])); got != size {
			t.Errorf("%s is %s, want %s", path, got, size)
		}
	}
	resp, manifest := r.page("/manifest.webmanifest", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{`"display": "standalone"`, `"start_url": "/"`, `"/icon-192.png", "sizes": "192x192"`, `"/icon-512.png", "sizes": "512x512"`, `"purpose": "maskable"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest lacks %s", want)
		}
	}

	_, page := r.page("/", reader)
	for _, want := range []string{
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<meta name="theme-color"`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-title" content="Annals">`,
		`<meta name="apple-mobile-web-app-status-bar-style"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the board's head lacks %s", want)
		}
	}
}
