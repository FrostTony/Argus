package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// BenchmarkWriteCheckResult is the answer to /api/check?capture=true: the
// page goes out as base64 inside the JSON.
func BenchmarkWriteCheckResult(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 1 << 20} {
		page := []byte(strings.Repeat("<p>x</p>", size/8))
		res := &app.CheckResult{Probe: "p", Type: "http", Success: true, Targets: []app.TargetCheck{{
			Target: "https://example.com/", Up: 2, Total: 2,
			Backends: []app.BackendCheck{
				{Backend: "1.1.1.1:443", Success: true, Artifacts: []probe.Artifact{{Name: "body", Data: page}}},
				{Backend: "1.0.0.1:443", Success: true, Artifacts: []probe.Artifact{{Name: "body", Data: page}}},
			},
		}}}
		for name, write := range map[string]func(http.ResponseWriter, int, any){
			"indented": writeJSON, "compact": writeCompactJSON,
		} {
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, name), func(b *testing.B) {
				b.SetBytes(int64(2 * size))
				b.ReportAllocs()
				for b.Loop() {
					write(httptest.NewRecorder(), http.StatusOK, res)
				}
			})
		}
	}
}
