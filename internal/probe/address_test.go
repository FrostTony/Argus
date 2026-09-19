package probe

import (
	"net/netip"
	"strings"
	"testing"
)

func TestRequestAddress(t *testing.T) {
	target := Target{Name: "site", Host: "site.example", Port: 8443}
	backend := Backend{Addr: netip.MustParseAddr("1.2.3.4"), Port: 443}

	cases := []struct {
		name  string
		req   Request
		ports []int
		want  string
	}{
		{
			name:  "the chosen backend wins",
			req:   Request{Target: target, Backend: backend},
			ports: []int{0, backend.Port, target.Port},
			want:  "1.2.3.4:443",
		},
		{
			name:  "the prober's own port wins over the backend's",
			req:   Request{Target: target, Backend: backend},
			ports: []int{25, backend.Port, target.Port},
			want:  "1.2.3.4:25",
		},
		{
			name:  "without fan-out the target's name is dialled",
			req:   Request{Target: target},
			ports: []int{0, 0, target.Port},
			want:  "site.example:8443",
		},
		{
			name:  "a default port applies last",
			req:   Request{Target: Target{Name: "site", Host: "site.example"}},
			ports: []int{0, 0, 0, 443},
			want:  "site.example:443",
		},
		{
			name:  "an IPv6 backend is bracketed",
			req:   Request{Target: target, Backend: Backend{Addr: netip.MustParseAddr("2606:4700::1"), Port: 443}},
			ports: []int{0, 443},
			want:  "[2606:4700::1]:443",
		},
	}
	for _, c := range cases {
		got, err := c.req.Address(c.ports...)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRequestAddressWithoutAPort(t *testing.T) {
	req := Request{Target: Target{Name: "site", Host: "site.example"}}
	_, err := req.Address(0, 0, 0)
	if err == nil {
		t.Fatal("a request with no port produced an address")
	}
	if got := ReasonOf(err); got != ReasonInternal {
		t.Fatalf("reason = %q, want %q", got, ReasonInternal)
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("the message does not mention the port: %v", err)
	}
}
