package quota

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func day(n int) *config.Quota { return &config.Quota{Requests: n, Per: config.PerDay} }

var noon = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func TestReserveIsAllOrNothing(t *testing.T) {
	a := Open("", quiet).Account("p", day(5), time.UTC)
	if ok, u := a.Reserve(3, noon); !ok || u.Used != 3 {
		t.Fatalf("first reservation: ok=%v used=%d", ok, u.Used)
	}
	// Three more do not fit in the two left, and none of them are taken.
	if ok, u := a.Reserve(3, noon); ok || u.Used != 3 {
		t.Fatalf("over-quota reservation: ok=%v used=%d, want refused with 3 used", ok, u.Used)
	}
	if ok, u := a.Reserve(2, noon); !ok || u.Used != 5 || u.Remaining() != 0 {
		t.Fatalf("exact fit: ok=%v used=%d", ok, u.Used)
	}
}

func TestSettleChargesWhatWasSent(t *testing.T) {
	a := Open("", quiet).Account("p", day(10), time.UTC)
	a.Reserve(4, noon)
	a.Settle(4, 1, noon) // three checks never ran
	if u := a.Usage(noon); u.Used != 1 {
		t.Fatalf("after a refund used = %d, want 1", u.Used)
	}
	a.Reserve(2, noon)
	a.Settle(2, 5, noon) // redirect hops
	if u := a.Usage(noon); u.Used != 6 {
		t.Fatalf("after a surcharge used = %d, want 6", u.Used)
	}
}

func TestPeriodTurns(t *testing.T) {
	a := Open("", quiet).Account("p", day(2), time.UTC)
	a.Reserve(2, noon)
	if ok, _ := a.Reserve(1, noon.Add(11*time.Hour)); ok {
		t.Fatal("the day's quota was handed out twice")
	}
	ok, u := a.Reserve(1, noon.Add(12*time.Hour))
	if !ok || u.Used != 1 {
		t.Fatalf("next day: ok=%v used=%d, want a fresh period", ok, u.Used)
	}
	if want := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC); !u.Resets.Equal(want) {
		t.Fatalf("resets %s, want %s", u.Resets, want)
	}
	// A run reserved yesterday and settled today leaves today alone.
	a.Settle(1, 3, noon)
	if u := a.Usage(noon.Add(12 * time.Hour)); u.Used != 1 {
		t.Fatalf("a late settlement reached the new period: used %d", u.Used)
	}
}

func TestChangedPeriodStartsOver(t *testing.T) {
	l := Open("", quiet)
	l.Account("p", day(10), time.UTC).Reserve(7, noon)
	monthly := l.Account("p", &config.Quota{Requests: 10, Per: config.PerMonth}, time.UTC)
	if u := monthly.Usage(noon); u.Used != 0 {
		t.Fatalf("a day's count carried into a month: used %d", u.Used)
	}
	// A new limit over the same period keeps what was used.
	l.Account("p", day(3), time.UTC).Reserve(1, noon)
	if u := l.Account("p", day(100), time.UTC).Usage(noon); u.Used != 1 {
		t.Fatalf("a raised limit lost the count: used %d", u.Used)
	}
}

func TestLedgerSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "quota.json")
	Open(path, quiet).Account("p", day(10), time.UTC).Reserve(4, noon)

	again := Open(path, quiet).Account("p", day(10), time.UTC)
	if u := again.Usage(noon); u.Used != 4 {
		t.Fatalf("after a restart used = %d, want 4", u.Used)
	}
	// Nothing but the ledger is left in the directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory holds %d files, want the ledger alone", len(entries))
	}
}

func TestUnreadableLedgerStartsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	l := Open(path, slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "unreadable") {
		t.Fatalf("a broken ledger was not reported: %q", logs.String())
	}
	if ok, _ := l.Account("p", day(1), time.UTC).Reserve(1, noon); !ok {
		t.Fatal("a broken ledger kept the probe from running")
	}
}

func TestNoQuotaIsNoAccount(t *testing.T) {
	var a *Account = Open("", quiet).Account("p", nil, time.UTC)
	if a != nil {
		t.Fatal("a probe without a quota got an account")
	}
	// Every method takes the nil account as unlimited.
	if ok, _ := a.Reserve(1_000_000, noon); !ok {
		t.Fatal("a nil account refused")
	}
	a.Settle(1, 2, noon)
	if u := a.Usage(noon); u.Limit != 0 {
		t.Fatalf("a nil account reported %+v", u)
	}
}

func TestReserveUnderContention(t *testing.T) {
	l := Open("", quiet)
	acct := l.Account("p", &config.Quota{Requests: 100, Per: config.PerDay}, time.UTC)
	now := time.Now()
	var granted atomic.Int64
	var wg sync.WaitGroup
	// Scheduled runs and on-demand checks of one probe race for the same count.
	for range 400 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := acct.Reserve(1, now); ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if g, u := granted.Load(), acct.Usage(now).Used; g != 100 || u != 100 {
		t.Fatalf("granted %d, used %d; want exactly the 100 the quota has", g, u)
	}
}
