package main

// PR archive: a full, offline copy of every PR you authored, one JSON file
// (raw gh payload + inline review comments) and one readable markdown file per
// PR under <prArchiveDir>/<repo>/. The week logs only keep a one-line summary;
// this keeps the description, commits, files, reviews and discussion so the
// work can still be read after repo access is gone.
//
// Refresh follows the week window: PRs updated since the window start are
// refetched and rewritten only when their content changed. A project with no
// archive yet gets every PR fetched once, whatever the window.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const prDetailFields = "number,title,body,state,isDraft,author,url,createdAt,mergedAt,closedAt," +
	"headRefName,baseRefName,additions,deletions,changedFiles,labels,files,commits,comments,reviews"

type ghUser struct {
	Login string `json:"login"`
}

type prDetail struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	IsDraft      bool   `json:"isDraft"`
	Author       ghUser `json:"author"`
	URL          string `json:"url"`
	CreatedAt    string `json:"createdAt"`
	MergedAt     string `json:"mergedAt"`
	ClosedAt     string `json:"closedAt"`
	HeadRefName  string `json:"headRefName"`
	BaseRefName  string `json:"baseRefName"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
	Labels       []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Files []struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
	Commits []struct {
		Oid             string `json:"oid"`
		MessageHeadline string `json:"messageHeadline"`
		AuthoredDate    string `json:"authoredDate"`
	} `json:"commits"`
	Comments []struct {
		Author    ghUser `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"createdAt"`
	} `json:"comments"`
	Reviews []struct {
		Author      ghUser `json:"author"`
		State       string `json:"state"`
		Body        string `json:"body"`
		SubmittedAt string `json:"submittedAt"`
	} `json:"reviews"`
	ReviewComments []reviewComment `json:"reviewComments"`
}

// reviewComment is an inline (line-anchored) review comment. gh pr view does
// not expose these, so they come from the REST API.
type reviewComment struct {
	User      string `json:"user"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type archiveStats struct {
	written, unchanged int
	warns              []string
}

// archiveProject refreshes one project's archive. listed is the window-scoped
// PR list the week logs already fetched.
func archiveProject(repo, ghAuthor, root string, listed []ghPR, dryRun bool) archiveStats {
	var st archiveStats
	dir := filepath.Join(root, repoSlug(repo))
	nums := make([]int, 0, len(listed))
	for _, pr := range listed {
		nums = append(nums, pr.Number)
	}
	if !hasArchive(dir) {
		all, err := ghPRList(repo, ghAuthor, "")
		if err != nil {
			st.warns = append(st.warns, fmt.Sprintf("%s: full PR list for archive failed: %v", repo, err))
			return st
		}
		if len(all) == ghPRLimit {
			st.warns = append(st.warns, fmt.Sprintf("%s: archive PR list hit %d, older PRs may be missing", repo, ghPRLimit))
		}
		nums = nums[:0]
		for _, pr := range all {
			nums = append(nums, pr.Number)
		}
	}
	if len(nums) == 0 {
		return st
	}
	if !dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			st.warns = append(st.warns, fmt.Sprintf("%s: %v", repo, err))
			return st
		}
	}
	for _, n := range nums {
		raw, pr, err := fetchPRDetail(repo, n)
		if err != nil {
			st.warns = append(st.warns, fmt.Sprintf("%s#%d: archive fetch failed: %v", repo, n, err))
			continue
		}
		base := filepath.Join(dir, strconv.Itoa(n))
		changed := false
		for path, data := range map[string][]byte{
			base + ".json": raw,
			base + ".md":   []byte(renderPRMarkdown(pr)),
		} {
			if old, _ := os.ReadFile(path); bytes.Equal(old, data) {
				continue
			}
			changed = true
			if dryRun {
				continue
			}
			if err := writeFileAtomic(path, data); err != nil {
				st.warns = append(st.warns, fmt.Sprintf("write %s: %v", path, err))
			}
		}
		if !changed {
			st.unchanged++
			continue
		}
		verb := "archived"
		if dryRun {
			verb = "would archive"
		}
		fmt.Printf("%s %s#%d\n", verb, repoSlug(repo), n)
		st.written++
	}
	return st
}

// fetchPRDetail returns the archived JSON (gh payload plus reviewComments,
// keys sorted so reruns are byte-stable) and its decoded form.
func fetchPRDetail(repo string, n int) ([]byte, prDetail, error) {
	var pr prDetail
	out, err := runCmd("gh", "pr", "view", strconv.Itoa(n), "-R", repo, "--json", prDetailFields)
	if err != nil {
		return nil, pr, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, pr, err
	}
	rcOut, err := runCmd("gh", "api", "--paginate",
		fmt.Sprintf("repos/%s/pulls/%d/comments", repo, n),
		"--jq", ".[] | {user: .user.login, path, line, body, createdAt: .created_at}")
	if err != nil {
		return nil, pr, fmt.Errorf("review comments: %w", err)
	}
	rcs, err := parseReviewComments(rcOut)
	if err != nil {
		return nil, pr, fmt.Errorf("review comments: %w", err)
	}
	if doc["reviewComments"], err = json.Marshal(rcs); err != nil {
		return nil, pr, err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, pr, err
	}
	raw = append(raw, '\n')
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, pr, err
	}
	return raw, pr, nil
}

// parseReviewComments reads the NDJSON that gh api --jq emits (one object per
// line across all pages).
func parseReviewComments(out []byte) ([]reviewComment, error) {
	rcs := []reviewComment{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var rc reviewComment
		if err := dec.Decode(&rc); err != nil {
			return nil, err
		}
		rcs = append(rcs, rc)
	}
	return rcs, nil
}

func renderPRMarkdown(pr prDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# #%d %s\n\n", pr.Number, pr.Title)
	state := pr.State
	if pr.IsDraft {
		state += " (draft)"
	}
	fmt.Fprintf(&b, "- State: %s\n", state)
	fmt.Fprintf(&b, "- Author: %s\n", pr.Author.Login)
	fmt.Fprintf(&b, "- Branch: %s <- %s\n", pr.BaseRefName, pr.HeadRefName)
	fmt.Fprintf(&b, "- Opened: %s\n", etTime(pr.CreatedAt))
	switch {
	case pr.MergedAt != "":
		fmt.Fprintf(&b, "- Merged: %s\n", etTime(pr.MergedAt))
	case pr.ClosedAt != "":
		fmt.Fprintf(&b, "- Closed: %s\n", etTime(pr.ClosedAt))
	}
	fmt.Fprintf(&b, "- Size: %s, %s, %s\n", statStr(pr.Additions, pr.Deletions),
		plural(pr.ChangedFiles, "file"), plural(len(pr.Commits), "commit"))
	if len(pr.Labels) > 0 {
		names := make([]string, len(pr.Labels))
		for i, l := range pr.Labels {
			names[i] = l.Name
		}
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "- URL: %s\n", pr.URL)

	body := strings.TrimSpace(pr.Body)
	if body == "" {
		body = "_(no description)_"
	}
	fmt.Fprintf(&b, "\n## Description\n\n%s\n", body)

	if len(pr.Commits) > 0 {
		b.WriteString("\n## Commits\n\n")
		for _, c := range pr.Commits {
			fmt.Fprintf(&b, "- %s %s\n", shortOid(c.Oid), c.MessageHeadline)
		}
	}
	if len(pr.Files) > 0 {
		b.WriteString("\n## Files\n\n")
		for _, f := range pr.Files {
			fmt.Fprintf(&b, "- %s %s\n", f.Path, statStr(f.Additions, f.Deletions))
		}
	}

	// Reviews, conversation comments and inline comments read best as one
	// timeline.
	type entry struct{ at, head, body string }
	var tl []entry
	for _, r := range pr.Reviews {
		tl = append(tl, entry{r.SubmittedAt, fmt.Sprintf("%s reviewed: %s", r.Author.Login, r.State), r.Body})
	}
	for _, c := range pr.Comments {
		tl = append(tl, entry{c.CreatedAt, c.Author.Login + " commented", c.Body})
	}
	for _, c := range pr.ReviewComments {
		loc := c.Path
		if c.Line != nil {
			loc += ":" + strconv.Itoa(*c.Line)
		}
		tl = append(tl, entry{c.CreatedAt, fmt.Sprintf("%s on `%s`", c.User, loc), c.Body})
	}
	sort.SliceStable(tl, func(i, j int) bool { return tl[i].at < tl[j].at })
	if len(tl) > 0 {
		b.WriteString("\n## Discussion\n")
		for _, e := range tl {
			fmt.Fprintf(&b, "\n### %s (%s)\n", e.head, etTime(e.at))
			if s := strings.TrimSpace(e.body); s != "" {
				fmt.Fprintf(&b, "\n%s\n", s)
			}
		}
	}
	return b.String()
}

// etTime renders an RFC3339 timestamp as "YYYY-MM-DD HH:MM ET".
func etTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(et).Format("2006-01-02 15:04") + " ET"
}

func shortOid(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}

// repoSlug names a project's archive folder after its repo ("owner/name" ->
// "name"), which is stable even if the config's display name changes.
func repoSlug(repo string) string {
	return repo[strings.LastIndex(repo, "/")+1:]
}

func hasArchive(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return len(m) > 0
}
