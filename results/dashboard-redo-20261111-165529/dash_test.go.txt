package promq

import (
	"os"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/promqltest"
)

func TestDashboard(t *testing.T) {
	b, err := os.ReadFile("dash.test")
	if err != nil {
		t.Fatal(err)
	}
	promqltest.RunTest(t, string(b), promqltest.NewTestEngine(t, false, 5*time.Minute, 100000))
}
