package retain

import (
	"fmt"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02_15-04-05", s)
	if err != nil {
		panic(err)
	}
	return t
}

// daily builds one backup per day for n days ending at end.
func daily(end time.Time, n int) []Item {
	var items []Item
	for i := 0; i < n; i++ {
		t := end.AddDate(0, 0, -i)
		items = append(items, Item{
			Name: t.Format("2006-01-02") + "_21-00-00_UTC.dump.tar.gz",
			Time: t,
		})
	}
	return items
}

// The newest backup must survive every policy. There is no configuration in
// which deleting the most recent backup is correct.
func TestNewestIsAlwaysKept(t *testing.T) {
	end := day("2026-08-09_21-00-00")
	for _, n := range []int{1, 2, 7, 30, 365, 1000} {
		for _, p := range []Policy{
			DefaultPolicy,
			{Daily: 1},
			{Monthly: 1},
			{Weekly: 1},
			{Daily: 1, Weekly: 1, Monthly: 1},
		} {
			items := daily(end, n)
			keep, _ := Select(items, p)
			if len(keep) == 0 {
				t.Fatalf("n=%d policy=%+v kept nothing", n, p)
			}
			newest := items[0]
			found := false
			for _, k := range keep {
				if k.Name == newest.Name {
					found = true
				}
			}
			if !found {
				t.Fatalf("n=%d policy=%+v deleted the newest backup %q", n, p, newest.Name)
			}
		}
	}
}

// Everything is either kept or pruned, never both and never lost.
func TestPartitionIsComplete(t *testing.T) {
	items := daily(day("2026-08-09_21-00-00"), 400)
	keep, prune := Select(items, DefaultPolicy)

	if len(keep)+len(prune) != len(items) {
		t.Fatalf("partition lost items: %d + %d != %d", len(keep), len(prune), len(items))
	}
	seen := map[string]int{}
	for _, i := range keep {
		seen[i.Name]++
	}
	for _, i := range prune {
		seen[i.Name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("%s appears %d times across keep+prune", name, count)
		}
	}
}

// A year of nightly dumps should collapse to roughly daily+weekly+monthly, not
// stay at 365.
func TestYearOfDailiesCollapses(t *testing.T) {
	items := daily(day("2026-08-09_21-00-00"), 365)
	keep, prune := Select(items, DefaultPolicy)

	if len(keep) > 25 {
		t.Errorf("expected ~23 kept under 7/4/12, got %d", len(keep))
	}
	if len(keep) < 15 {
		t.Errorf("kept suspiciously few: %d", len(keep))
	}
	if len(prune) == 0 {
		t.Error("expected a year of dailies to prune something")
	}
	t.Logf("365 dailies -> keep %d, prune %d", len(keep), len(prune))
}

// Fewer backups than the policy allows means nothing is deleted.
func TestUnderPolicyKeepsEverything(t *testing.T) {
	items := daily(day("2026-08-09_21-00-00"), 5)
	keep, prune := Select(items, DefaultPolicy)
	if len(prune) != 0 {
		t.Fatalf("expected no pruning with 5 backups under 7 daily, pruned %d", len(prune))
	}
	if len(keep) != 5 {
		t.Fatalf("expected all 5 kept, got %d", len(keep))
	}
}

func TestEmptyAndSingle(t *testing.T) {
	if keep, prune := Select(nil, DefaultPolicy); keep != nil || prune != nil {
		t.Fatal("empty input should produce empty output")
	}
	one := daily(day("2026-08-09_21-00-00"), 1)
	keep, prune := Select(one, DefaultPolicy)
	if len(keep) != 1 || len(prune) != 0 {
		t.Fatalf("single backup: keep=%d prune=%d, want 1/0", len(keep), len(prune))
	}
}

// Several backups on the same day: the newest of that day represents it.
func TestMultiplePerDay(t *testing.T) {
	items := []Item{
		{Name: "a", Time: day("2026-08-09_08-00-00")},
		{Name: "b", Time: day("2026-08-09_21-00-00")},
		{Name: "c", Time: day("2026-08-08_21-00-00")},
	}
	keep, _ := Select(items, Policy{Daily: 2})
	names := map[string]bool{}
	for _, k := range keep {
		names[k.Name] = true
	}
	if !names["b"] {
		t.Error("newest of the day (b) must be kept")
	}
	if !names["c"] {
		t.Error("previous day (c) must be kept")
	}
}

func TestParseTimestamp(t *testing.T) {
	got, err := ParseTimestamp("2026-08-07_21-00-00_UTC.dump.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := day("2026-08-07_21-00-00")
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Names we cannot parse must error, never yield a zero time that would look
	// ancient and get pruned.
	for _, bad := range []string{"tusk-send", "random.dump", "", "backup.tar.gz", "2026-13-45_99-99-99_UTC.dump"} {
		if ts, err := ParseTimestamp(bad); err == nil {
			t.Errorf("ParseTimestamp(%q) unexpectedly succeeded with %v", bad, ts)
		}
	}
}

// Pruning must be idempotent: re-running over the kept set deletes nothing new.
func TestSelectIsStable(t *testing.T) {
	items := daily(day("2026-08-09_21-00-00"), 200)
	keep1, _ := Select(items, DefaultPolicy)
	keep2, prune2 := Select(keep1, DefaultPolicy)

	if len(prune2) != 0 {
		t.Fatalf("second pass pruned %d already-kept items: %v", len(prune2), prune2[0].Name)
	}
	if len(keep1) != len(keep2) {
		t.Fatalf("unstable: %d then %d", len(keep1), len(keep2))
	}
}

// 90 nightly dumps under 7/4/12 collapse to 13: the 7 most recent days, plus
// three older week-representatives and three older month-representatives. The
// newest entry of the current week and month is already covered by the dailies,
// which is why the total is not 7+4+12.
func ExampleSelect() {
	items := daily(day("2026-08-09_21-00-00"), 90)
	keep, prune := Select(items, DefaultPolicy)
	fmt.Printf("keep=%d prune=%d", len(keep), len(prune))
	// Output: keep=13 prune=77
}
