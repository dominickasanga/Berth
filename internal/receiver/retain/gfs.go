// Package retain decides which backups to keep.
//
// This is the only code in the system that deletes data, so it is written as a
// pure function over timestamps: given the same inputs it always returns the
// same decision, and it can be tested exhaustively without touching a disk.
// Anything it cannot confidently classify is kept.
package retain

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Policy is a grandfather-father-son retention rule.
type Policy struct {
	Daily   int // most recent N distinct days
	Weekly  int // most recent N distinct ISO weeks
	Monthly int // most recent N distinct months
}

var DefaultPolicy = Policy{Daily: 7, Weekly: 4, Monthly: 12}

// Item is one backup under consideration.
type Item struct {
	Name string
	Time time.Time
}

// Select partitions items into those to keep and those to prune.
//
// An item is kept if it is the newest in one of the most recent N days, ISO
// weeks, or months. The newest item overall is therefore always kept: a policy
// that could delete the latest backup would be a bug with no safe failure mode.
func Select(items []Item, p Policy) (keep, prune []Item) {
	if len(items) == 0 {
		return nil, nil
	}

	// Newest first, so the first item seen for any period is the one to keep.
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Time.Equal(sorted[b].Time) {
			return sorted[a].Name > sorted[b].Name
		}
		return sorted[a].Time.After(sorted[b].Time)
	})

	keepNames := make(map[string]bool, len(sorted))
	claim := func(limit int, key func(time.Time) string) {
		if limit <= 0 {
			return
		}
		seen := make(map[string]bool, limit)
		for _, it := range sorted {
			k := key(it.Time)
			if seen[k] {
				continue // an older item from a period already represented
			}
			if len(seen) >= limit {
				break // sorted newest-first, so every later period is older
			}
			seen[k] = true
			keepNames[it.Name] = true
		}
	}

	claim(p.Daily, func(t time.Time) string {
		return t.UTC().Format("2006-01-02")
	})
	claim(p.Weekly, func(t time.Time) string {
		y, w := t.UTC().ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	})
	claim(p.Monthly, func(t time.Time) string {
		return t.UTC().Format("2006-01")
	})

	for _, it := range sorted {
		if keepNames[it.Name] {
			keep = append(keep, it)
		} else {
			prune = append(prune, it)
		}
	}
	return keep, prune
}

// timestampPattern matches the naming the Django backup job produces, e.g.
// "2026-08-07_21-00-00_UTC.dump.tar.gz".
var timestampPattern = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})_(\d{2}-\d{2}-\d{2})_([A-Za-z]+)`)

// ParseTimestamp extracts the backup time from a filename.
//
// Returns an error rather than guessing when the name does not match: an
// unparseable name must never be treated as "very old" and deleted.
func ParseTimestamp(name string) (time.Time, error) {
	m := timestampPattern.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return time.Time{}, fmt.Errorf("no timestamp in %q", name)
	}
	t, err := time.Parse("2006-01-02_15-04-05", m[1]+"_"+m[2])
	if err != nil {
		return time.Time{}, fmt.Errorf("bad timestamp in %q: %w", name, err)
	}
	return t, nil
}
