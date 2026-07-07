package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWeekMonday(t *testing.T) {
	cases := map[string]string{
		"20260511": "20260511", // Monday -> itself
		"20260513": "20260511", // Wednesday
		"20260517": "20260511", // Sunday
	}
	for in, want := range cases {
		mon, err := weekMonday(parseDate(in))
		if err != nil {
			t.Fatalf("weekMonday(%s): %v", in, err)
		}
		if got := mon.Format(dateLayout); got != want {
			t.Errorf("weekMonday(%s) = %s, want %s", in, got, want)
		}
	}
	if _, err := weekMonday(time.Time{}); err == nil {
		t.Error("weekMonday(zero) should error")
	}
}

func TestPREvents(t *testing.T) {
	base := ghPR{Number: 7, Title: "fix login", State: "MERGED",
		HeadRefName: "fix-login", BaseRefName: "qa", Additions: 10, Deletions: 2}

	t.Run("same-day merge collapses", func(t *testing.T) {
		pr := base
		pr.CreatedAt = "2026-06-01T10:00:00Z"
		pr.MergedAt = "2026-06-01T15:00:00Z"
		evs := prEvents(pr, "2026-05-11", "")
		if len(evs) != 1 || evs[0].date != "20260601" {
			t.Fatalf("got %+v", evs)
		}
		want := "- #7 opened & merged: fix login (qa <- fix-login) (+10 -2)"
		if evs[0].line != want {
			t.Errorf("line = %q, want %q", evs[0].line, want)
		}
	})

	t.Run("split days", func(t *testing.T) {
		pr := base
		pr.CreatedAt = "2026-06-01T10:00:00Z"
		pr.MergedAt = "2026-06-03T15:00:00Z"
		evs := prEvents(pr, "2026-05-11", "")
		if len(evs) != 2 || evs[0].date != "20260601" || evs[1].date != "20260603" {
			t.Fatalf("got %+v", evs)
		}
	})

	t.Run("open event before cutoff dropped, merge kept", func(t *testing.T) {
		pr := base
		pr.CreatedAt = "2026-05-01T10:00:00Z"
		pr.MergedAt = "2026-06-03T15:00:00Z"
		evs := prEvents(pr, "2026-05-11", "")
		if len(evs) != 1 || evs[0].date != "20260603" {
			t.Fatalf("got %+v", evs)
		}
	})

	t.Run("closed unmerged", func(t *testing.T) {
		pr := base
		pr.State = "CLOSED"
		pr.CreatedAt = "2026-06-01T10:00:00Z"
		pr.ClosedAt = "2026-06-01T12:00:00Z"
		evs := prEvents(pr, "2026-05-11", "")
		if len(evs) != 1 {
			t.Fatalf("got %+v", evs)
		}
		want := "- #7 opened & closed (unmerged): fix login (qa <- fix-login) (+10 -2)"
		if evs[0].line != want {
			t.Errorf("line = %q", evs[0].line)
		}
	})

	t.Run("still open", func(t *testing.T) {
		pr := base
		pr.State = "OPEN"
		pr.CreatedAt = "2026-06-01T10:00:00Z"
		evs := prEvents(pr, "2026-05-11", "")
		if len(evs) != 1 || evs[0].date != "20260601" {
			t.Fatalf("got %+v", evs)
		}
	})
}

func TestDominantBranch(t *testing.T) {
	qa := []string{"qa"}
	cases := []struct {
		name      string
		tally     map[string]int
		preferred []string
		want      string
	}{
		{"empty", map[string]int{}, qa, "main"},
		{"main only", map[string]int{"main": 5}, qa, "main"},
		{"count wins", map[string]int{"feat-x": 3, "qa": 1}, qa, "feat-x"},
		{"preferred breaks tie", map[string]int{"feat-x": 2, "qa": 2}, qa, "qa"},
		{"shorter name breaks tie", map[string]int{"feature-long": 1, "dev": 1}, qa, "dev"},
		{"alpha breaks final tie", map[string]int{"bbb": 1, "aaa": 1}, qa, "aaa"},
		{"custom preferred", map[string]int{"staging": 1, "dev": 1}, []string{"staging"}, "staging"},
		{"no preference falls back to length", map[string]int{"qa": 1, "dev": 1}, nil, "qa"},
	}
	for _, c := range cases {
		if got := dominantBranch(c.tally, c.preferred); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSortPRLines(t *testing.T) {
	lines := []string{"- #12 merged: b", "- #3 opened: a", "- #3 merged: a"}
	sortPRLines(lines)
	want := []string{"- #3 opened: a", "- #3 merged: a", "- #12 merged: b"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("got %v", lines)
	}
}

func TestTrimBlock(t *testing.T) {
	got := trimBlock([]string{"", "  ", "a", "", "b", "", ""})
	if got != "a\n\nb" {
		t.Errorf("got %q", got)
	}
	if trimBlock([]string{"", "  "}) != "" {
		t.Error("all-blank should be empty")
	}
}

func TestParseRepoURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/nyu/ulc-scheduler.git\n": "nyu/ulc-scheduler",
		"git@github.com:nyu/ulc-scheduler.git":       "nyu/ulc-scheduler",
		"ssh://git@github.com/nyu/repo":              "nyu/repo",
	}
	for in, want := range cases {
		got, err := parseRepoURL(in)
		if err != nil || got != want {
			t.Errorf("parseRepoURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseRepoURL("not-a-url"); err == nil {
		t.Error("expected error for unparseable remote")
	}
}

func TestParseFile(t *testing.T) {
	content := `# 20260601-20260605 (Week 4)

week intro here

## Mon - 20260601

day-level note

### ULC

<!-- daily-auto:start ULC 20260601 -->
(on qa branch)

commits:

- #abc123 auto stuff (+1 -0)
<!-- daily-auto:end ULC 20260601 -->

manual note under ULC

### Gone Project

only manual content
`
	notes := map[string]string{}
	intros := map[string]string{}
	parseFile(content, notes, intros)

	if got := intros["20260601"]; got != "week intro here" {
		t.Errorf("intro = %q", got)
	}
	if got := notes["20260601|"]; got != "day-level note" {
		t.Errorf("day note = %q", got)
	}
	if got := notes["20260601|ULC"]; got != "manual note under ULC" {
		t.Errorf("ULC note = %q (auto block must be excluded)", got)
	}
	if got := notes["20260601|Gone Project"]; got != "only manual content" {
		t.Errorf("extra project note = %q", got)
	}
}

func TestParseFileInvalidDateHeader(t *testing.T) {
	content := `## Mon - 20260601

### ULC

see ticket ## ref 99999999 details
`
	notes := map[string]string{}
	parseFile(content, notes, map[string]string{})
	if got := notes["20260601|ULC"]; got != "see ticket ## ref 99999999 details" {
		t.Errorf("ULC note = %q; 8-digit non-date must stay note text", got)
	}
}

func TestRenderSection(t *testing.T) {
	sec := &section{
		tally:   map[string]int{"qa": 2, "main": 2},
		prs:     []string{"- #5 merged: x (qa <- f) (+1 -1)"},
		commits: []string{"- #abc fix (+1 -0)"},
	}
	got := renderSection(sec, []string{"qa"})
	want := "(on qa branch)\n\nPRs:\n\n- #5 merged: x (qa <- f) (+1 -1)\n\ncommits:\n\n- #abc fix (+1 -0)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := renderSection(&section{tally: map[string]int{}}, nil); got != "(no recorded activity)\n" {
		t.Errorf("empty section = %q", got)
	}
}

func TestPREventsLinksAndFlags(t *testing.T) {
	pr := ghPR{Number: 7, Title: "fix login", State: "MERGED",
		HeadRefName: "fix-login", BaseRefName: "qa", Additions: 10, Deletions: 2,
		CreatedAt: "2026-06-01T10:00:00Z", MergedAt: "2026-06-03T15:00:00Z"}
	evs := prEvents(pr, "2026-05-11", "https://github.com/o/r")
	if len(evs) != 2 {
		t.Fatalf("got %+v", evs)
	}
	wantOpen := "- [#7](https://github.com/o/r/pull/7) opened: fix login (qa <- fix-login) (+10 -2)"
	if evs[0].line != wantOpen {
		t.Errorf("open line = %q", evs[0].line)
	}
	if !evs[0].opened || evs[0].merged {
		t.Errorf("open flags = %+v", evs[0])
	}
	if evs[1].opened || !evs[1].merged {
		t.Errorf("merge flags = %+v", evs[1])
	}
}

func TestLinkHash(t *testing.T) {
	if got := linkHash("", "abc123"); got != "#abc123" {
		t.Errorf("plain = %q", got)
	}
	if got := linkHash("https://github.com/o/r", "abc123"); got != "[#abc123](https://github.com/o/r/commit/abc123)" {
		t.Errorf("linked = %q", got)
	}
}

func TestWeekStatLine(t *testing.T) {
	ws := &weekStat{commits: 23, add: 2134, del: 812, opened: 1, merged: 5}
	want := "stats: 23 commits (+2134 -812), 1 PR opened, 5 merged"
	if got := ws.line(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFetchSince(t *testing.T) {
	first := parseDate("20260511")
	now := parseDate("20260706") // a Monday
	fmtd := func(tm time.Time) string { return tm.Format(dateLayout) }

	// -all wins regardless of files.
	if got := fetchSince([]string{"20260629-20260703.md"}, first, true, now); fmtd(got) != "20260511" {
		t.Errorf("all = %s", fmtd(got))
	}
	// No files yet: full backfill.
	if got := fetchSince(nil, first, false, now); fmtd(got) != "20260511" {
		t.Errorf("fresh = %s", fmtd(got))
	}
	// Up-to-date files: previous week's Monday.
	if got := fetchSince([]string{"20260629-20260703.md", "20260706-20260710.md"}, first, false, now); fmtd(got) != "20260629" {
		t.Errorf("recent = %s", fmtd(got))
	}
	// Gap since last run: window extends back to newest file's week.
	if got := fetchSince([]string{"20260601-20260605.md"}, first, false, now); fmtd(got) != "20260601" {
		t.Errorf("gap = %s", fmtd(got))
	}
	// Never earlier than the first week.
	if got := fetchSince([]string{"20260504-20260508.md"}, first, false, now); fmtd(got) != "20260511" {
		t.Errorf("clamp = %s", fmtd(got))
	}
}

func TestWeekStatFromFile(t *testing.T) {
	content := `# 20260601-20260605 (Week 4)

<!-- daily-auto:start week 20260601 -->
stats: 23 commits (+2134 -812), 3 PRs opened, 5 merged
<!-- daily-auto:end week 20260601 -->

intro
`
	if got := weekStatFromFile(content); got != "23 commits (+2134 -812), 3 PRs opened, 5 merged" {
		t.Errorf("got %q", got)
	}
	if got := weekStatFromFile("# old file\n\n## Mon - 20260601\n"); got != "" {
		t.Errorf("old-format file should have no stats, got %q", got)
	}
}

func TestRenderWeekBlocks(t *testing.T) {
	cfg := &Config{FirstWeekStart: "20260511", PreferredBranches: []string{"qa"},
		Projects: []Project{{Name: "ULC"}}}
	monday := parseDate("20260601")
	first := parseDate("20260511")
	days := map[string]bool{"20260601": true}
	sections := map[string]*section{"20260601|ULC": {
		tally:   map[string]int{"qa": 1},
		commits: []string{"- [#abc](u/commit/abc) fix (+1 -0)"},
	}}
	genDays := map[string]map[string]bool{"20260601": {"ULC": true}}
	dayTimes := map[string]*timeSpan{"20260601": {min: "10:32", max: "17:45"}}

	got := renderWeek(monday, first, cfg, days, sections, genDays,
		map[string]string{}, "", "stats: 1 commit (+1 -0), 0 PRs opened, 0 merged", dayTimes)

	for _, want := range []string{
		"# 20260601-20260605 (Week 4)",
		"<!-- daily-auto:start week 20260601 -->\nstats: 1 commit (+1 -0), 0 PRs opened, 0 merged\n<!-- daily-auto:end week 20260601 -->",
		"<!-- daily-auto:start day 20260601 -->\n(commit activity 10:32 - 17:45)\n<!-- daily-auto:end day 20260601 -->",
		"- [#abc](u/commit/abc) fix (+1 -0)",
	} {
		if !contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// No stats / no dayTimes: neither block appears.
	bare := renderWeek(monday, first, cfg, days, sections, genDays,
		map[string]string{}, "", "", map[string]*timeSpan{})
	if contains(bare, "daily-auto:start week") || contains(bare, "daily-auto:start day") {
		t.Errorf("unexpected auto blocks:\n%s", bare)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
