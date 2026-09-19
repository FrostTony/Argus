package server

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

// logoPNG is the status page's mark, embedded so offline nodes render it too.
//
//go:embed logo.png
var logoPNG []byte

// logoModTime is fixed so a browser can revalidate the embedded asset.
var logoModTime = time.Date(2026, time.September, 19, 0, 0, 0, 0, time.UTC)

func serveLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "logo.png", logoModTime, bytes.NewReader(logoPNG))
}
