package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
)

// statusPage renders the node's current state; trends accumulate in the browser.
type statusPage struct {
	app  *app.App
	tmpl *template.Template
}

func newStatusPage(a *app.App) (*statusPage, error) {
	t, err := template.New("status").Parse(statusHTML)
	if err != nil {
		return nil, err
	}
	return &statusPage{app: a, tmpl: t}, nil
}

// handle serves the page with the first snapshot already embedded in it.
func (s *statusPage) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/status") {
		http.NotFound(w, r)
		return
	}
	state := s.state()
	initial, err := json.Marshal(state)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Initial lands in a JS string literal, where html/template escapes it.
	data := struct {
		State   stateView
		Initial string
	}{State: state, Initial: string(initial)}
	if err := s.tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// data serves a fresh snapshot in the shape the page was first rendered with.
func (s *statusPage) data(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if err := enc.Encode(s.state()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
