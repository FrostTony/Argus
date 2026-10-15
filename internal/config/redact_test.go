package config

import (
	"strings"
	"testing"
)

func TestRedactMasksCredentials(t *testing.T) {
	doc := `probes:
  - name: api
    type: http
    targets: ["https://alice:hunter2@example.com/health"]
    http:
      headers: {Authorization: "Bearer HEADER-SECRET", X-Api-Key: KEY-SECRET, Accept: text/html}
      proxy_url: http://proxyuser:PROXY-SECRET@10.0.0.1:3128
      auth:
        basic: {username: monitor, password: BASIC-SECRET}
        bearer: {token: "${API_TOKEN}"}
        oauth2: {client_id: argus, client_secret: OAUTH-SECRET, token_url: https://idp.example.com/token, client_secret_file: /etc/argus/secret}
---
probes: [{name: b, type: http, targets: ["https://x.example.com/"], http: {auth: {bearer: {token: SECOND-DOC-SECRET}}}}]
`
	got := string(Redact([]byte(doc)))
	for _, leaked := range []string{"hunter2", "HEADER-SECRET", "KEY-SECRET", "PROXY-SECRET", "BASIC-SECRET", "OAUTH-SECRET", "SECOND-DOC-SECRET"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%s survived:\n%s", leaked, got)
		}
	}
	// What a reader needs to understand the check stays.
	for _, kept := range []string{"${API_TOKEN}", "username: monitor", "https://idp.example.com/token", "/etc/argus/secret", "Accept: text/html", "alice:xxxxx@example.com"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was lost:\n%s", kept, got)
		}
	}
}

func TestTargetNameHidesThePassword(t *testing.T) {
	var tg Target
	if err := tg.Parse("https://alice:hunter2@example.com/health"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tg.Name, "hunter2") || tg.URL != "https://alice:hunter2@example.com/health" {
		t.Errorf("name %q, url %q: the name is a label, the URL is what is dialled", tg.Name, tg.URL)
	}
}
