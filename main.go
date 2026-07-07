// daily-gen generates weekly work-log markdown files from git + GitHub history.
//
// Usage: daily-gen [-n] [-all] [config.json]
//
// It reads config.json (projects, author identities, first-week anchor), walks
// each project's git history for the configured authors, pulls your PRs via the
// gh CLI, groups everything by ISO week and weekday, and writes one markdown
// file per week (e.g. 20260601-20260605.md) plus an INDEX.md summary table.
// Projects are collected in parallel.
//
// By default only recent weeks are fetched and re-rendered: from the Monday of
// the previous week, extended back to the newest week file on disk so gaps
// from skipped runs are backfilled. Older files are left untouched. -all
// refetches and re-renders everything since firstWeekStart. -n reports which
// files would change without writing.
//
// Auto-generated content lives inside <!-- daily-auto:start ... --> /
// <!-- daily-auto:end ... --> marker blocks. Anything you write outside those
// markers (prose, "(on qa branch)" tweaks, extra notes) is preserved verbatim
// on every re-run. Running again is incremental: a week's file is only rewritten
// when that week's derived content actually changed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- config ----

type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Config struct {
	Authors           []string  `json:"authors"`        // git author emails (commits)
	GithubAuthor      string    `json:"githubAuthor"`   // gh --author value, default "@me"
	FirstWeekStart    string    `json:"firstWeekStart"` // YYYYMMDD, anchors "Week 1"
	OutputDir         string    `json:"outputDir"`
	PreferredBranches []string  `json:"preferredBranches"` // branch-guess tie-break order, default ["qa"]
	Projects          []Project `json:"projects"`
}

const dateLayout = "20060102"

// ---- models ----

type commit struct {
	hash     string
	dateStr  string // YYYYMMDD
	hhmm     string // HH:MM author time, feeds the day activity span
	isMerge  bool
	subject  string
	add, del int
	branches []string
}

type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"` // OPEN, MERGED, CLOSED
	CreatedAt   string `json:"createdAt"`
	MergedAt    string `json:"mergedAt"`
	ClosedAt    string `json:"closedAt"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
}

// section is the generated content for one (day, project) cell.
type section struct {
	tally   map[string]int // branch name -> # of this cell's commits on it
	prs     []string       // formatted PR bullet lines
	commits []string       // formatted "- #hash subject" bullets
}

// projectData is everything collected for one project, gathered concurrently.
type projectData struct {
	name     string
	repoURL  string // https://github.com/owner/repo, "" if no GitHub remote
	commits  []commit
	prEvents []prEvent
	warns    []string
	err      error
}

// timeSpan is the first/last commit time of a day ("HH:MM", lexically ordered).
type timeSpan struct {
	min, max string
}

// weekStat accumulates the weekly summary line.
type weekStat struct {
	commits, add, del int
	opened, merged    int
}

func (w *weekStat) line() string {
	return fmt.Sprintf("stats: %s (+%d -%d), %s opened, %d merged",
		plural(w.commits, "commit"), w.add, w.del, plural(w.opened, "PR"), w.merged)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func main() {
	dryRun := flag.Bool("n", false, "dry run: report files that would change without writing")
	all := flag.Bool("all", false, "refetch and re-render every week since firstWeekStart")
	flag.Parse()
	cfgPath := "config.json"
	if flag.NArg() > 0 {
		cfgPath = flag.Arg(0)
	}
	cfg, baseDir, err := loadConfig(cfgPath)
	if err != nil {
		fail(err)
	}

	firstMonday, err := weekMonday(parseDate(cfg.FirstWeekStart))
	if err != nil {
		fail(fmt.Errorf("bad firstWeekStart %q: %w", cfg.FirstWeekStart, err))
	}
	outDir := resolve(baseDir, cfg.OutputDir)
	sinceMonday := fetchSince(listWeekFiles(outDir), firstMonday, *all, time.Now())
	since := sinceMonday.Format("2006-01-02") // git --since + PR-event lower bound

	results := make([]projectData, len(cfg.Projects))
	var wg sync.WaitGroup
	for i, p := range cfg.Projects {
		wg.Go(func() {
			results[i] = collectProject(p, baseDir, cfg.Authors, cfg.GithubAuthor, since)
		})
	}
	wg.Wait()

	sections := map[string]*section{}       // "20260601|ULC" -> section
	genDays := map[string]map[string]bool{} // "20260601" -> set of projects with activity
	dayTimes := map[string]*timeSpan{}      // "20260601" -> commit activity span
	weekStats := map[string]*weekStat{}     // monday(YYYYMMDD) -> weekly totals

	week := func(date string) *weekStat {
		mon, err := weekMonday(parseDate(date))
		if err != nil {
			return &weekStat{} // generated dates are always valid; discard if not
		}
		mk := mon.Format(dateLayout)
		if weekStats[mk] == nil {
			weekStats[mk] = &weekStat{}
		}
		return weekStats[mk]
	}
	cell := func(date, proj string) *section {
		key := date + "|" + proj
		s := sections[key]
		if s == nil {
			s = &section{tally: map[string]int{}}
			sections[key] = s
		}
		if genDays[date] == nil {
			genDays[date] = map[string]bool{}
		}
		genDays[date][proj] = true
		return s
	}

	for _, r := range results {
		for _, w := range r.warns {
			fmt.Fprintln(os.Stderr, "warn: "+w)
		}
		if r.err != nil {
			fail(r.err)
		}
		// Merge commits are housekeeping; skip them — PRs cover merges.
		for _, c := range r.commits {
			if c.isMerge {
				continue
			}
			sec := cell(c.dateStr, r.name)
			sec.commits = append(sec.commits, "- "+linkHash(r.repoURL, c.hash)+" "+c.subject+" "+statStr(c.add, c.del))
			for _, b := range c.branches {
				sec.tally[b]++
			}
			ws := week(c.dateStr)
			ws.commits++
			ws.add += c.add
			ws.del += c.del
			if c.hhmm != "" {
				if ts := dayTimes[c.dateStr]; ts == nil {
					dayTimes[c.dateStr] = &timeSpan{c.hhmm, c.hhmm}
				} else {
					ts.min = min(ts.min, c.hhmm)
					ts.max = max(ts.max, c.hhmm)
				}
			}
		}
		for _, ev := range r.prEvents {
			sec := cell(ev.date, r.name)
			sec.prs = append(sec.prs, ev.line)
			ws := week(ev.date)
			if ev.opened {
				ws.opened++
			}
			if ev.merged {
				ws.merged++
			}
		}
	}
	for _, sec := range sections {
		sortPRLines(sec.prs)
	}

	// Parse existing week files to recover manual notes + week intros.
	manualNotes := map[string]string{} // "date|project" ("" project = day-level)
	weekIntros := map[string]string{}  // monday(YYYYMMDD) -> intro text
	if err := parseExistingFiles(outDir, manualNotes, weekIntros); err != nil {
		fail(err)
	}

	// Union of weeks to (re)emit: weeks with activity + weeks already on disk.
	weekDays := map[string]map[string]bool{} // monday -> set of day dates
	addDay := func(dateStr string) {
		mon, err := weekMonday(parseDate(dateStr))
		if err != nil {
			return
		}
		mk := mon.Format(dateLayout)
		if weekDays[mk] == nil {
			weekDays[mk] = map[string]bool{}
		}
		weekDays[mk][dateStr] = true
	}
	for d := range genDays {
		addDay(d)
	}
	for k := range manualNotes {
		addDay(strings.SplitN(k, "|", 2)[0])
	}

	if !*dryRun {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			fail(err)
		}
	}
	sinceMK := sinceMonday.Format(dateLayout)
	written, unchanged := 0, 0
	for mk, days := range weekDays {
		if !*all && mk < sinceMK {
			continue // outside the fetch window: data is partial, leave the file alone
		}
		monday := parseDate(mk)
		var stat string
		if ws := weekStats[mk]; ws != nil {
			stat = ws.line()
		}
		content := renderWeek(monday, firstMonday, cfg, days, sections, genDays, manualNotes, weekIntros[mk], stat, dayTimes)
		path := filepath.Join(outDir, weekFilename(monday))
		old, _ := os.ReadFile(path)
		if string(old) == content {
			unchanged++
			continue
		}
		if *dryRun {
			fmt.Printf("would update %s\n", filepath.Base(path))
		} else {
			if err := writeFileAtomic(path, []byte(content)); err != nil {
				fail(fmt.Errorf("write %s: %w", path, err))
			}
			fmt.Printf("updated %s\n", filepath.Base(path))
		}
		written++
	}
	// INDEX.md summary table, rebuilt from the week files on disk. In a dry
	// run the week files were not rewritten, so this reflects pre-run state.
	if idx := renderIndex(outDir, firstMonday); idx != "" {
		path := filepath.Join(outDir, "INDEX.md")
		old, _ := os.ReadFile(path)
		if string(old) != idx {
			if *dryRun {
				fmt.Println("would update INDEX.md")
			} else {
				if err := writeFileAtomic(path, []byte(idx)); err != nil {
					fail(fmt.Errorf("write INDEX.md: %w", err))
				}
				fmt.Println("updated INDEX.md")
			}
		}
	}

	if *dryRun {
		fmt.Printf("done (dry run): %d would change, %d unchanged\n", written, unchanged)
	} else {
		fmt.Printf("done: %d written, %d unchanged\n", written, unchanged)
	}
}

// fetchSince picks the fetch/re-render window start given the week files
// already on disk. Default: the previous week's Monday, extended back to the
// newest week file so gaps from skipped runs are backfilled; a fresh output
// dir (or -all) fetches everything since firstMonday.
func fetchSince(weekFiles []string, firstMonday time.Time, all bool, now time.Time) time.Time {
	if all || len(weekFiles) == 0 {
		return firstMonday
	}
	newest := time.Time{}
	for _, name := range weekFiles {
		if d := parseDate(name[:8]); d.After(newest) {
			newest = d
		}
	}
	today := parseDate(now.Format(dateLayout))
	mon, _ := weekMonday(today) // today is never zero
	start := mon.AddDate(0, 0, -7)
	if newest.Before(start) {
		start = newest
	}
	if start.Before(firstMonday) {
		start = firstMonday
	}
	return start
}

// listWeekFiles returns the YYYYMMDD-YYYYMMDD.md filenames in outDir, sorted.
func listWeekFiles(outDir string) []string {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && reFilename.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// collectProject gathers commits and PR events for one project. Only a failing
// git log is fatal; missing remotes, gh failures, and branch-lookup failures
// degrade to warnings.
func collectProject(p Project, baseDir string, authors []string, ghAuthor, since string) projectData {
	d := projectData{name: p.Name}
	projPath := resolve(baseDir, p.Path)

	// A bare date in git --since means "that date at the current time of day"
	// (approxidate), which silently drops the window day's earlier commits.
	gitSince := since + "T00:00:00"
	branches, err := branchMap(projPath, authors, gitSince)
	if err != nil {
		d.warns = append(d.warns, fmt.Sprintf("%s: branch lookup failed, branch guesses degraded: %v", p.Name, err))
	}
	d.commits, err = gitLog(projPath, authors, gitSince, branches)
	if err != nil {
		d.err = fmt.Errorf("git log %s: %w", p.Name, err)
		return d
	}

	// Pull requests via gh (authoritative for your PRs, incl. squash-merged).
	repo, err := gitRemoteRepo(projPath)
	if err != nil {
		d.warns = append(d.warns, fmt.Sprintf("%s: no github remote, skipping PRs: %v", p.Name, err))
		return d
	}
	d.repoURL = "https://github.com/" + repo
	prs, err := ghPRList(repo, ghAuthor, since)
	if err != nil {
		d.warns = append(d.warns, fmt.Sprintf("%s: gh pr list failed, skipping PRs: %v", p.Name, err))
		return d
	}
	if len(prs) == ghPRLimit {
		d.warns = append(d.warns, fmt.Sprintf("%s: gh returned exactly %d PRs, results may be truncated", p.Name, ghPRLimit))
	}
	for _, pr := range prs {
		d.prEvents = append(d.prEvents, prEvents(pr, since, d.repoURL)...)
	}
	return d
}

// ---- subprocess helpers ----

// runCmd runs a command and returns stdout; on failure the captured stderr is
// folded into the error so "exit status 1" is actually diagnosable.
func runCmd(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func runGit(projPath string, args ...string) ([]byte, error) {
	return runCmd("git", append([]string{"-C", projPath}, args...)...)
}

// ---- git ----

const fieldSep = "\x1f"

func gitLog(projPath string, authors []string, since string, branches map[string][]string) ([]commit, error) {
	args := []string{
		"log", "--branches", "--remotes", "--no-color", "--numstat",
		"--date=format:%Y-%m-%d %H:%M",
		"--pretty=format:%h" + fieldSep + "%ad" + fieldSep + "%P" + fieldSep + "%s",
		"--since=" + since,
	}
	for _, a := range authors {
		args = append(args, "--author="+a)
	}
	out, err := runGit(projPath, args...)
	if err != nil {
		return nil, err
	}
	var commits []commit
	var cur *commit
	flush := func() {
		if cur != nil {
			commits = append(commits, *cur)
		}
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, fieldSep) { // commit header
			flush()
			parts := strings.SplitN(line, fieldSep, 4)
			if len(parts) != 4 {
				cur = nil
				continue
			}
			t, err := time.Parse("2006-01-02 15:04", parts[1])
			if err != nil {
				cur = nil
				continue
			}
			cur = &commit{
				hash:     parts[0],
				dateStr:  t.Format(dateLayout),
				hhmm:     t.Format("15:04"),
				isMerge:  len(strings.Fields(parts[2])) > 1,
				subject:  parts[3],
				branches: branches[parts[0]],
			}
			continue
		}
		if cur != nil && line != "" { // numstat: added\tdeleted\tpath ("-" = binary)
			f := strings.SplitN(line, "\t", 3)
			if len(f) >= 2 {
				cur.add += atoiSafe(f[0])
				cur.del += atoiSafe(f[1])
			}
		}
	}
	flush()
	return commits, nil
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func statStr(add, del int) string {
	return fmt.Sprintf("(+%d -%d)", add, del)
}

// branchMap returns commit hash -> normalized branch names (origin/ prefix
// stripped, HEAD dropped, deduped) for the "(on <branch> branch)" best-guess.
// It runs one git log per ref instead of one git branch --contains per commit,
// which is dramatically cheaper on long histories.
func branchMap(projPath string, authors []string, since string) (map[string][]string, error) {
	out, err := runGit(projPath, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	sets := map[string]map[string]bool{}
	for ref := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		ref = strings.TrimSpace(ref)
		name := strings.TrimPrefix(ref, "origin/")
		if ref == "" || name == "HEAD" || strings.HasSuffix(name, "/HEAD") {
			continue
		}
		args := []string{"log", ref, "--no-color", "--pretty=format:%h", "--since=" + since}
		for _, a := range authors {
			args = append(args, "--author="+a)
		}
		lout, err := runGit(projPath, args...)
		if err != nil {
			continue // broken ref; the branch guess is best-effort
		}
		for h := range strings.FieldsSeq(string(lout)) {
			if sets[h] == nil {
				sets[h] = map[string]bool{}
			}
			sets[h][name] = true
		}
	}
	res := make(map[string][]string, len(sets))
	for h, set := range sets {
		res[h] = sortedKeys(set)
	}
	return res, nil
}

// gitRemoteRepo extracts "owner/repo" from the origin remote URL.
func gitRemoteRepo(projPath string) (string, error) {
	out, err := runGit(projPath, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return parseRepoURL(string(out))
}

func parseRepoURL(raw string) (string, error) {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if i := strings.Index(s, "github.com"); i >= 0 {
		s = strings.TrimLeft(s[i+len("github.com"):], ":/")
	}
	if !strings.Contains(s, "/") {
		return "", fmt.Errorf("cannot parse repo from %q", raw)
	}
	return s, nil
}

func dominantBranch(tally map[string]int, preferred []string) string {
	rank := func(name string) int {
		for i, p := range preferred {
			if name == p {
				return i
			}
		}
		return len(preferred)
	}
	type cand struct {
		name string
		n    int
	}
	var cands []cand
	for b, n := range tally {
		if b == "main" || b == "master" {
			continue
		}
		cands = append(cands, cand{b, n})
	}
	if len(cands) == 0 {
		return "main"
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.n != b.n {
			return a.n > b.n
		}
		if ra, rb := rank(a.name), rank(b.name); ra != rb {
			return ra < rb
		}
		if len(a.name) != len(b.name) {
			return len(a.name) < len(b.name)
		}
		return a.name < b.name
	})
	return cands[0].name
}

// ---- gh / PRs ----

const ghPRLimit = 300

func ghPRList(repo, author, sinceISO string) ([]ghPR, error) {
	// updated:>= is a superset of every PR with a created/merged/closed event
	// in range (all of those bump updatedAt), and keeps the payload small.
	out, err := runCmd("gh", "pr", "list", "-R", repo,
		"--author", author, "--state", "all", "--limit", strconv.Itoa(ghPRLimit),
		"--search", "updated:>="+sinceISO,
		"--json", "number,title,state,createdAt,mergedAt,closedAt,headRefName,baseRefName,additions,deletions")
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

type prEvent struct {
	date           string // YYYYMMDD
	line           string
	opened, merged bool // feed the weekly stats line
}

// prEvents expands a PR into dated "- #N <verb>: ..." log lines. Same-day
// open+close collapse into one line; otherwise opened and merged/closed land on
// their own days. Events before minISO (YYYY-MM-DD) are dropped.
func prEvents(pr ghPR, minISO, repoURL string) []prEvent {
	suffix := fmt.Sprintf("%s (%s <- %s) %s", pr.Title, pr.BaseRefName, pr.HeadRefName, statStr(pr.Additions, pr.Deletions))
	open := iso10(pr.CreatedAt)
	bullet := func(verb string) string {
		return fmt.Sprintf("- %s %s: %s", linkPR(repoURL, pr.Number), verb, suffix)
	}
	var out []prEvent
	add := func(isoDate, verb string, opened, merged bool) {
		if isoDate == "" || isoDate < minISO {
			return
		}
		out = append(out, prEvent{
			date: strings.ReplaceAll(isoDate, "-", ""), line: bullet(verb),
			opened: opened, merged: merged,
		})
	}
	switch pr.State {
	case "MERGED":
		m := iso10(pr.MergedAt)
		if m == open {
			add(open, "opened & merged", true, true)
		} else {
			add(open, "opened", true, false)
			add(m, "merged", false, true)
		}
	case "CLOSED":
		c := iso10(pr.ClosedAt)
		if c == open {
			add(open, "opened & closed (unmerged)", true, false)
		} else {
			add(open, "opened", true, false)
			add(c, "closed (unmerged)", false, false)
		}
	default: // OPEN
		add(open, "opened", true, false)
	}
	return out
}

// linkPR / linkHash render the "#x" token, hyperlinked when the project has a
// GitHub remote.
func linkPR(repoURL string, num int) string {
	if repoURL == "" {
		return fmt.Sprintf("#%d", num)
	}
	return fmt.Sprintf("[#%d](%s/pull/%d)", num, repoURL, num)
}

func linkHash(repoURL, hash string) string {
	if repoURL == "" {
		return "#" + hash
	}
	return fmt.Sprintf("[#%s](%s/commit/%s)", hash, repoURL, hash)
}

func iso10(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

var rePRNum = regexp.MustCompile(`#(\d+)`)

func sortPRLines(lines []string) {
	num := func(s string) int {
		if m := rePRNum.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	sort.SliceStable(lines, func(i, j int) bool { return num(lines[i]) < num(lines[j]) })
}

// ---- rendering ----

func renderWeek(monday, firstMonday time.Time, cfg *Config, days map[string]bool,
	sections map[string]*section, genDays map[string]map[string]bool,
	manualNotes map[string]string, intro, stat string, dayTimes map[string]*timeSpan) string {

	var b strings.Builder
	mk := monday.Format(dateLayout)
	friday := monday.AddDate(0, 0, 4)
	fmt.Fprintf(&b, "# %s-%s (Week %d)\n\n", mk, friday.Format(dateLayout), weekNumber(monday, firstMonday))
	if stat != "" {
		fmt.Fprintf(&b, "<!-- daily-auto:start week %s -->\n%s\n<!-- daily-auto:end week %s -->\n\n", mk, stat, mk)
	}

	// "first week" note only on the anchor week, unless you've written your own.
	if intro == "" && monday.Equal(firstMonday) {
		intro = "(" + cfg.FirstWeekStart + " is my first week)"
	}
	if intro != "" {
		b.WriteString(intro)
		b.WriteString("\n\n")
	}

	for _, d := range sortedKeys(days) {
		dt := parseDate(d)
		fmt.Fprintf(&b, "## %s - %s\n\n", dt.Weekday().String()[:3], d)
		if ts := dayTimes[d]; ts != nil {
			span := ts.min
			if ts.max != ts.min {
				span += " - " + ts.max
			}
			fmt.Fprintf(&b, "<!-- daily-auto:start day %s -->\n(commit activity %s)\n<!-- daily-auto:end day %s -->\n\n", d, span, d)
		}
		if note := manualNotes[d+"|"]; note != "" {
			b.WriteString(note)
			b.WriteString("\n\n")
		}
		for _, proj := range orderedProjects(d, cfg, genDays, manualNotes) {
			fmt.Fprintf(&b, "### %s\n\n", proj)
			key := d + "|" + proj
			if sec := sections[key]; sec != nil {
				fmt.Fprintf(&b, "<!-- daily-auto:start %s %s -->\n", proj, d)
				b.WriteString(renderSection(sec, cfg.PreferredBranches))
				fmt.Fprintf(&b, "<!-- daily-auto:end %s %s -->\n\n", proj, d)
			}
			if note := manualNotes[key]; note != "" {
				b.WriteString(note)
				b.WriteString("\n\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderSection(sec *section, preferred []string) string {
	var lines []string
	if len(sec.tally) > 0 {
		lines = append(lines, "(on "+dominantBranch(sec.tally, preferred)+" branch)")
	}
	if len(sec.prs) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "PRs:", "")
		lines = append(lines, sec.prs...)
	}
	if len(sec.commits) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "commits:", "")
		lines = append(lines, sec.commits...)
	}
	if len(lines) == 0 {
		lines = append(lines, "(no recorded activity)")
	}
	return strings.Join(lines, "\n") + "\n"
}

func weekNumber(monday, firstMonday time.Time) int {
	return int(math.Round(monday.Sub(firstMonday).Hours()/24/7)) + 1
}

// renderIndex builds the INDEX.md summary table from the week files on disk,
// one row per week with the stats line pulled from its week auto block.
// Returns "" when there are no week files.
func renderIndex(outDir string, firstMonday time.Time) string {
	names := listWeekFiles(outDir)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Index\n\n<!-- generated by daily-gen, do not edit -->\n\n")
	b.WriteString("| Week | File | Stats |\n| --- | --- | --- |\n")
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(outDir, n))
		if err != nil {
			continue
		}
		monday := parseDate(n[:8])
		fmt.Fprintf(&b, "| %d | [%s - %s](%s) | %s |\n",
			weekNumber(monday, firstMonday), n[:8], n[9:17], n, weekStatFromFile(string(data)))
	}
	return b.String()
}

// weekStatFromFile extracts the stats text from a week file's week-level auto
// block ("" for files that predate stats).
func weekStatFromFile(content string) string {
	in := false
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(line, "<!-- daily-auto:start week") {
			in = true
			continue
		}
		if in {
			if s, ok := strings.CutPrefix(line, "stats: "); ok {
				return s
			}
			if strings.HasPrefix(line, "<!--") {
				return ""
			}
		}
	}
	return ""
}

func orderedProjects(date string, cfg *Config, genDays map[string]map[string]bool, manualNotes map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range cfg.Projects {
		if genDays[date][p.Name] || manualNotes[date+"|"+p.Name] != "" {
			out = append(out, p.Name)
			seen[p.Name] = true
		}
	}
	var extras []string
	for k := range manualNotes {
		parts := strings.SplitN(k, "|", 2)
		if parts[0] == date && parts[1] != "" && !seen[parts[1]] {
			extras = append(extras, parts[1])
			seen[parts[1]] = true
		}
	}
	sort.Strings(extras)
	return append(out, extras...)
}

// ---- parsing existing files (manual-note preservation) ----

var (
	reDayHdr   = regexp.MustCompile(`^##\s+.*?(\d{8})`)
	reProjHdr  = regexp.MustCompile(`^###\s+(.+?)\s*$`)
	reAutoMark = regexp.MustCompile(`^<!--\s*daily-auto:(start|end)\b`)
	reFilename = regexp.MustCompile(`^\d{8}-\d{8}\.md$`)
)

func parseExistingFiles(outDir string, manualNotes, weekIntros map[string]string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !reFilename.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		if err != nil {
			return err
		}
		parseFile(string(data), manualNotes, weekIntros)
	}
	return nil
}

func parseFile(content string, manualNotes, weekIntros map[string]string) {
	var introBuf, noteBuf []string
	curDate, curProj := "", ""
	firstMonday := ""
	seenDay := false
	inAuto := false

	keep := func(line string) {
		if !seenDay {
			introBuf = append(introBuf, line)
		} else {
			noteBuf = append(noteBuf, line)
		}
	}
	flushNote := func() {
		if curDate == "" {
			return
		}
		if txt := trimBlock(noteBuf); txt != "" {
			manualNotes[curDate+"|"+curProj] = txt
		}
		noteBuf = nil
	}

	for line := range strings.SplitSeq(content, "\n") {
		switch {
		case reAutoMark.MatchString(line):
			inAuto = strings.Contains(line, ":start")
			continue
		case inAuto:
			continue
		case strings.HasPrefix(line, "# "):
			continue // title is regenerated
		case reDayHdr.MatchString(line):
			d := reDayHdr.FindStringSubmatch(line)[1]
			if parseDate(d).IsZero() { // 8 digits but not a real date: keep as text
				keep(line)
				continue
			}
			flushNote()
			seenDay = true
			curDate = d
			curProj = ""
			if firstMonday == "" {
				if mon, err := weekMonday(parseDate(curDate)); err == nil {
					firstMonday = mon.Format(dateLayout)
				}
			}
			continue
		case reProjHdr.MatchString(line):
			flushNote()
			curProj = reProjHdr.FindStringSubmatch(line)[1]
			continue
		default:
			keep(line)
		}
	}
	flushNote()
	if firstMonday != "" {
		if txt := trimBlock(introBuf); txt != "" {
			weekIntros[firstMonday] = txt
		}
	}
}

func trimBlock(lines []string) string {
	i, j := 0, len(lines)
	for i < j && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	for j > i && strings.TrimSpace(lines[j-1]) == "" {
		j--
	}
	return strings.Join(lines[i:j], "\n")
}

// ---- helpers ----

func loadConfig(path string) (*Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, "", err
	}
	if cfg.GithubAuthor == "" {
		cfg.GithubAuthor = "@me"
	}
	if cfg.PreferredBranches == nil {
		cfg.PreferredBranches = []string{"qa"}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	return &cfg, filepath.Dir(abs), nil
}

func resolve(baseDir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

func parseDate(s string) time.Time {
	t, _ := time.Parse(dateLayout, s)
	return t
}

func weekMonday(t time.Time) (time.Time, error) {
	if t.IsZero() {
		return t, fmt.Errorf("zero date")
	}
	daysFromMon := (int(t.Weekday()) + 6) % 7 // Mon=0 ... Sun=6
	return t.AddDate(0, 0, -daysFromMon), nil
}

func weekFilename(monday time.Time) string {
	return monday.Format(dateLayout) + "-" + monday.AddDate(0, 0, 4).Format(dateLayout) + ".md"
}

// writeFileAtomic writes via a temp file + rename so an interrupted run can't
// leave a half-written week file (these files carry manual notes).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".daily-gen-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
