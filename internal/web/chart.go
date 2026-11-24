package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/monitor"
	"github.com/dominickasanga/berth/internal/store"
)

// chartSVG draws a metric as an inline SVG line chart (no JavaScript, no chart library).
// bucket is the spacing of the points; a bigger gap than a few buckets breaks the line.
func chartSVG(title string, pts []store.Point, unit string, from, to time.Time, bucket time.Duration) template.HTML {
	const W, H, left, right, top, bottom = 480.0, 130.0, 8.0, 8.0, 14.0, 20.0
	if len(pts) == 0 {
		return template.HTML(`<p class="mute" style="margin:12px 0">No data in this range yet.</p>`)
	}
	lo, hi := 0.0, 0.0
	for _, p := range pts {
		hi = math.Max(hi, p.V)
	}
	switch unit {
	case "pct":
		hi = 100
	case "bool":
		hi = 1
	default:
		if hi <= 0 {
			hi = 1
		}
		hi *= 1.15
	}
	span := to.Sub(from).Seconds()
	if span <= 0 {
		span = 1
	}
	x := func(t time.Time) float64 { return left + (W-left-right)*t.Sub(from).Seconds()/span }
	y := func(v float64) float64 { return top + (H-top-bottom)*(1-(v-lo)/(hi-lo)) }
	base := y(lo)

	var line, area, dots strings.Builder
	segStart := -1
	flush := func(end int) {
		if segStart < 0 {
			return
		}
		seg := pts[segStart : end+1]
		if len(seg) == 1 { // a lone point has no line: draw it as a dot
			fmt.Fprintf(&dots, `<circle class="dot" cx="%.1f" cy="%.1f" r="2.2"/>`, x(seg[0].T), y(seg[0].V))
		}
		for i, p := range seg {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x(p.T), y(p.V))
		}
		fmt.Fprintf(&area, "M%.1f %.1f ", x(seg[0].T), base)
		for _, p := range seg {
			fmt.Fprintf(&area, "L%.1f %.1f ", x(p.T), y(p.V))
		}
		fmt.Fprintf(&area, "L%.1f %.1f Z ", x(seg[len(seg)-1].T), base)
		segStart = -1
	}
	maxGap := 4 * bucket
	for i, p := range pts {
		if segStart >= 0 && p.T.Sub(pts[i-1].T) > maxGap {
			flush(i - 1)
		}
		if segStart < 0 {
			segStart = i
		}
	}
	flush(len(pts) - 1)

	tf := "15:04"
	switch {
	case span > 36*3600:
		tf = "Jan 2"
	case span >= 18*3600: // a day apart would otherwise print the same clock time at both ends
		tf = "Jan 2 15:04"
	}
	last := pts[len(pts)-1]
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chartsvg" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, html.EscapeString(title))
	for _, f := range []float64{0, 0.5, 1} { // grid
		gy := top + (H-top-bottom)*f
		fmt.Fprintf(&b, `<line class="grid" x1="%.0f" y1="%.1f" x2="%.0f" y2="%.1f"/>`, left, gy, W-right, gy)
	}
	fmt.Fprintf(&b, `<text class="ax" x="%.0f" y="%.1f">%s</text>`, left+2, top-3, html.EscapeString(monitor.Format(unit, hi)))
	fmt.Fprintf(&b, `<path class="area" d="%s"/><path class="ln" d="%s"/>`, strings.TrimSpace(area.String()), strings.TrimSpace(line.String()))
	b.WriteString(dots.String())
	fmt.Fprintf(&b, `<circle class="dot" cx="%.1f" cy="%.1f" r="2.6"/>`, x(last.T), y(last.V))
	fmt.Fprintf(&b, `<text class="ax" x="%.0f" y="%.0f">%s</text>`, left+2, H-5, from.Format(tf))
	fmt.Fprintf(&b, `<text class="ax" x="%.0f" y="%.0f" text-anchor="end">%s</text>`, W-right-2, H-5, to.Format(tf))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
