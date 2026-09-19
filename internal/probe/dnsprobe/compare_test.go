package dnsprobe

import (
	"testing"
)

func TestEmptyAnswerDisagreesWithAFullOne(t *testing.T) {
	lost := serve(t, zone{})
	good := serve(t, zone{answers: []string{"A 1.1.1.1"}})
	p := newProber(t, `
servers: ["`+lost+`", "`+good+`"]
compare_servers: true
min_answers: 0
`)
	passed, consistent := 0, 0
	const runs = 40
	for i := 0; i < runs; i++ {
		res, samples := run(t, p)
		if res.Err == nil {
			passed++
		}
		if v, _ := gauge(samples, "dns_consistent"); v == 1 {
			consistent++
		}
	}
	if passed > 0 || consistent > 0 {
		t.Fatalf("resolvers disagree ([] vs [1.1.1.1]), yet %d of %d runs passed and %d reported dns_consistent=1",
			passed, runs, consistent)
	}
}
