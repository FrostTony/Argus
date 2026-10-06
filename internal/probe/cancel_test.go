package probe

import (
	"context"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

type resolverFunc func(context.Context) (resolve.Result, error)

func (f resolverFunc) Resolve(ctx context.Context, _ string, _ resolve.Family) (resolve.Result, error) {
	return f(ctx)
}

// A run cancelled by shutdown, reload or a client hanging up says nothing about
// the backends: a healthy one must not turn down, nor later "recover".
func TestRunnerCancelledRunLeavesHealthAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(r *Runner, started chan<- struct{})
	}{
		{"probe", func(r *Runner, started chan<- struct{}) {
			r.Prober = proberFunc(func(ctx context.Context, _ Request, _ *metrics.Recorder) Result {
				started <- struct{}{}
				<-ctx.Done()
				return Result{Err: Wrap(ReasonConnect, ctx.Err())}
			})
		}},
		{"resolve", func(r *Runner, started chan<- struct{}) {
			r.Resolver = resolverFunc(func(ctx context.Context) (resolve.Result, error) {
				started <- struct{}{}
				<-ctx.Done()
				return resolve.Result{}, ctx.Err()
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(&fakeProber{}, "10.0.0.1")
			r.RunOnce(context.Background(), &collector{})
			if r.Down() != 0 {
				t.Fatalf("precondition: %d backends down", r.Down())
			}

			started := make(chan struct{})
			tc.block(r, started)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				<-started
				cancel()
			}()
			r.RunOnce(ctx, &collector{})

			if r.Down() != 0 || len(r.Failures()) != 0 {
				t.Fatalf("cancellation recorded as an outage: down=%d failures=%v", r.Down(), r.Failures())
			}
		})
	}
}
