package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

// cronPlan is what `argus cron` prints with -json: the same answers the node
// gives a probe on that cron, for whoever renders configurations and wants to
// show the runs before pushing — and to check its own arithmetic against.
type cronPlan struct {
	Cron     string   `json:"cron"`
	Timezone string   `json:"timezone"`
	MinGap   float64  `json:"min_gap_seconds"`
	Next     []string `json:"next"`
	// Runs counts the runs in [From, To), when both were given.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Runs *int   `json:"runs,omitempty"`
}

func cmdCron(args []string) error {
	fs := flag.NewFlagSet("cron", flag.ContinueOnError)
	tz := fs.String("tz", "", "IANA time zone the specification is read in; the local one by default")
	n := fs.Int("n", 5, "how many of the next runs to list")
	after := fs.String("after", "", "list the runs after this time (RFC 3339) rather than now")
	from := fs.String("from", "", "count the runs from this time (RFC 3339), inclusive")
	to := fs.String("to", "", "count the runs up to this time (RFC 3339), exclusive")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf(`usage: argus cron [-tz Europe/Moscow] [-n 5] [-after T] [-from T -to T] [-json] "0 * * * *" ["30 18 * * *" ...]`)
	}

	loc, err := (config.Probe{Timezone: *tz}).Location()
	if err != nil {
		return err
	}
	// Several specifications are one schedule, as a probe's cron: list is.
	c, err := config.ParseCrons(fs.Args(), loc)
	if err != nil {
		return err
	}
	start := time.Now()
	if *after != "" {
		if start, err = time.Parse(time.RFC3339, *after); err != nil {
			return fmt.Errorf("-after: %w", err)
		}
	}
	plan := cronPlan{Cron: c.String(), Timezone: loc.String(), MinGap: c.MinGap().Seconds(), Next: []string{}}
	for t, i := c.Next(start), 0; !t.IsZero() && i < *n; t, i = c.Next(t), i+1 {
		plan.Next = append(plan.Next, t.Format(time.RFC3339))
	}
	if *from != "" || *to != "" {
		f, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return fmt.Errorf("-from: %w", err)
		}
		u, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return fmt.Errorf("-to: %w", err)
		}
		runs := c.Runs(f, u)
		plan.From, plan.To, plan.Runs = *from, *to, &runs
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	fmt.Printf("%s in %s, at least %s between runs\n", plan.Cron, plan.Timezone, c.MinGap())
	for _, t := range plan.Next {
		fmt.Println("  " + t)
	}
	if plan.Runs != nil {
		fmt.Printf("%d run(s) from %s to %s\n", *plan.Runs, plan.From, plan.To)
	}
	return nil
}
