package web

import (
	"strings"
	"testing"
	"time"

	"github.com/dominickasanga/berth/internal/store"
)

func TestChartSVG(t *testing.T) {
	to := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	from := to.Add(-time.Hour)
	pt := func(minAgo int, v float64) store.Point {
		return store.Point{T: to.Add(-time.Duration(minAgo) * time.Minute), V: v}
	}

	if got := string(chartSVG("x", nil, "count", from, to, time.Minute)); !strings.Contains(got, "No data") {
		t.Errorf("empty series should say so: %s", got)
	}
	got := string(chartSVG(`conn <b>`, []store.Point{pt(50, 10), pt(40, 20), pt(30, 15), pt(5, 30)}, "count", from, to, 5*time.Minute))
	if !strings.Contains(got, "<svg") || !strings.Contains(got, `class="ln"`) || !strings.Contains(got, "11:00") || !strings.Contains(got, "12:00") {
		t.Fatalf("chart missing parts: %s", got)
	}
	if strings.Contains(got, "<b>") {
		t.Fatal("the title must be escaped")
	}
	// a gap longer than four buckets (here 25 minutes against 20) splits the line in two
	if n := strings.Count(strings.SplitN(strings.SplitN(got, `class="ln" d="`, 2)[1], `"`, 2)[0], "M"); n != 2 {
		t.Errorf("expected 2 line segments around the gap, got %d", n)
	}
	// percentages always use 0..100 so a flat 5% does not look like a spike
	flat := string(chartSVG("p", []store.Point{pt(10, 5), pt(9, 5)}, "pct", from, to, time.Minute))
	if !strings.Contains(flat, ">100.0%<") {
		t.Errorf("pct chart should be scaled to 100%%: %s", flat)
	}
	// a single point still draws
	if one := string(chartSVG("p", []store.Point{pt(1, 3)}, "bool", from, to, time.Minute)); !strings.Contains(one, "<circle") {
		t.Errorf("a single point should draw a dot: %s", one)
	}
}

func TestChartAxisLabelsShowTheDateOverADay(t *testing.T) {
	to := time.Date(2026, 11, 24, 11, 51, 0, 0, time.UTC)
	pts := []store.Point{{T: to.Add(-time.Hour), V: 1}, {T: to, V: 2}}
	for span, want := range map[time.Duration]string{time.Hour: "10:51", 24 * time.Hour: "Nov 23 11:51", 7 * 24 * time.Hour: "Nov 17"} {
		svg := string(chartSVG("x", pts, "count", to.Add(-span), to, time.Minute))
		if !strings.Contains(svg, ">"+want+"<") {
			t.Errorf("a %v chart should start with the label %q: %s", span, want, svg[len(svg)-160:])
		}
	}
}
