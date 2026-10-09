// Package branch reads where every branch of a git repo stands: where it is
// checked out and who works there, how far it is from the base branch and
// from the remote, and what is not committed yet. It only reads, and it
// takes no optional locks, so polling it never trips an agent's own git
// command in the same worktree. The remote figures are as of the last
// fetch or push.
package branch

import (
	"fmt"
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

// Who is someone working in a folder: an agent or a headless thread.
type Who struct{ Name, Dir string }

// Branch is one local branch, or a worktree with no branch of its own.
type Branch struct {
	Name     string   `json:"name"`
	Head     string   `json:"head,omitempty"` // short hash; empty before the first commit
	Subject  string   `json:"subject,omitempty"`
	When     int64    `json:"when,omitempty"`     // commit time of the head
	Worktree string   `json:"worktree,omitempty"` // where it is checked out
	Agents   []string `json:"agents,omitempty"`   // who works in that worktree
	Ahead    int      `json:"ahead"`              // its commits the base branch lacks
	Behind   int      `json:"behind"`             // base commits it lacks
	Upstream string   `json:"upstream,omitempty"`
	OnRemote bool     `json:"on_remote"`
	Gone     bool     `json:"gone,omitempty"` // its upstream was deleted
	Unpushed int      `json:"unpushed"`
	Unpulled int      `json:"unpulled"`
	Changed  int      `json:"changed"` // tracked files with uncommitted changes
	New      int      `json:"new"`     // untracked files
	Added    int      `json:"added"`   // uncommitted lines in tracked files
	Deleted  int      `json:"deleted"`
	IsBase   bool     `json:"base,omitempty"`
	Unborn   bool     `json:"unborn,omitempty"`   // checked out but no commit yet
	Detached bool     `json:"detached,omitempty"` // a worktree on no branch
	Missing  bool     `json:"missing,omitempty"`  // its worktree folder is gone
}

// Repo is a repo and all its branches, the base branch first.
type Repo struct {
	Root     string   `json:"root"` // the main worktree
	Base     string   `json:"base,omitempty"`
	Remote   string   `json:"remote,omitempty"`
	URL      string   `json:"url,omitempty"`
	Fetched  int64    `json:"fetched,omitempty"` // last fetch, so how fresh the remote figures are
	Branches []Branch `json:"branches"`
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"--no-optional-locks", "-C", dir}, args...)...).Output()
	return strings.TrimRight(string(out), "\n"), err
}

// Common returns the git dir that every worktree of dir's repo shares, so
// two folders are the same repo when their Common matches.
func Common(dir string) (string, bool) {
	out, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return out, err == nil && out != ""
}

// Fetch updates the remote's branches, so Scan compares against what is on
// the remote now rather than at the last fetch.
func Fetch(dir string) error {
	r := remote(dir)
	if r == "" {
		return nil
	}
	cmd := exec.Command("git", "-C", dir, "fetch", "--quiet", r)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		return fmt.Errorf("git fetch %s: timed out", r)
	}
}

type worktree struct {
	path, head, branch string
	detached, prunable bool
}

func worktrees(dir string) []worktree {
	out, err := git(dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil
	}
	var wts []worktree
	var w *worktree
	for _, f := range strings.Split(out, "\x00") {
		key, val, _ := strings.Cut(f, " ")
		switch key {
		case "worktree":
			wts = append(wts, worktree{path: val})
			w = &wts[len(wts)-1]
		case "HEAD":
			w.head = val
		case "branch":
			w.branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			w.detached = true
		case "prunable":
			w.prunable = true
		}
	}
	return wts
}

// remote is origin, or the only other remote there is.
func remote(dir string) string {
	out, _ := git(dir, "remote")
	names := strings.Fields(out)
	for _, n := range names {
		if n == "origin" {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func exists(dir, ref string) bool {
	_, err := git(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// base is the branch the others are measured against: the remote's
// default, then main or master, then whatever the main worktree is on.
// ref is what to compare with: the local branch, or the remote's copy when
// there is no local one.
func base(dir, rem string, wts []worktree) (name, ref string) {
	var names []string
	if rem != "" {
		if out, err := git(dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+rem+"/HEAD"); err == nil {
			names = append(names, strings.TrimPrefix(out, rem+"/"))
		}
	}
	names = append(names, "main", "master")
	if len(wts) > 0 && wts[0].branch != "" {
		names = append(names, wts[0].branch)
	}
	for _, n := range names {
		if exists(dir, "refs/heads/"+n) {
			return n, "refs/heads/" + n
		}
		if rem != "" && exists(dir, "refs/remotes/"+rem+"/"+n) {
			return n, "refs/remotes/" + rem + "/" + n
		}
	}
	return "", ""
}

// counts reads "a\tb" from rev-list --left-right --count.
func counts(dir, left, right string) (int, int) {
	out, err := git(dir, "rev-list", "--left-right", "--count", left+"..."+right)
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0
	}
	a, _ := strconv.Atoi(f[0])
	b, _ := strconv.Atoi(f[1])
	return a, b
}

var trackRe = regexp.MustCompile(`(ahead|behind) (\d+)`)

// Scan reads the repo dir is in, and puts each of who on the branch whose
// worktree it works in.
func Scan(dir string, who []Who) (Repo, error) {
	if _, ok := Common(dir); !ok {
		return Repo{}, fmt.Errorf("not a git repo: %s", dir)
	}
	wts := worktrees(dir)
	r := Repo{Root: dir}
	if len(wts) > 0 {
		r.Root = wts[0].path
	}
	common, _ := Common(r.Root)
	if fi, err := os.Stat(filepath.Join(common, "FETCH_HEAD")); err == nil {
		r.Fetched = fi.ModTime().Unix()
	}
	r.Remote = remote(r.Root)
	if r.Remote != "" {
		r.URL, _ = git(r.Root, "remote", "get-url", r.Remote)
	}
	var baseRef string
	r.Base, baseRef = base(r.Root, r.Remote, wts)

	onRemote := map[string]bool{}
	if r.Remote != "" {
		out, _ := git(r.Root, "for-each-ref", "--format=%(refname)", "refs/remotes/"+r.Remote)
		for _, ref := range strings.Fields(out) {
			onRemote[strings.TrimPrefix(ref, "refs/remotes/"+r.Remote+"/")] = true
		}
	}

	format := "%(refname:lstrip=2)%00%(objectname:short)%00%(committerdate:unix)%00%(subject)%00%(upstream:short)%00%(upstream:track,nobracket)"
	if baseRef != "" {
		format += "%00%(ahead-behind:" + baseRef + ")"
	}
	out, err := git(r.Root, "for-each-ref", "--format="+format, "refs/heads")
	if err != nil {
		return r, fmt.Errorf("git for-each-ref in %s: %v", r.Root, err)
	}
	byName := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 6 {
			continue
		}
		b := Branch{Name: f[0], Head: f[1], Subject: f[3], Upstream: f[4], IsBase: f[0] == r.Base}
		b.When, _ = strconv.ParseInt(f[2], 10, 64)
		if len(f) > 6 {
			fmt.Sscan(f[6], &b.Ahead, &b.Behind)
		}
		switch {
		case b.Upstream != "" && f[5] == "gone":
			b.Gone = true
		case b.Upstream != "":
			b.OnRemote = true
			for _, m := range trackRe.FindAllStringSubmatch(f[5], -1) {
				n, _ := strconv.Atoi(m[2])
				if m[1] == "ahead" {
					b.Unpushed = n
				} else {
					b.Unpulled = n
				}
			}
		case onRemote[b.Name]:
			b.OnRemote = true
			b.Unpushed, b.Unpulled = counts(r.Root, "refs/heads/"+b.Name, "refs/remotes/"+r.Remote+"/"+b.Name)
		}
		byName[b.Name] = len(r.Branches)
		r.Branches = append(r.Branches, b)
	}

	// Put each worktree on its branch; a worktree whose branch has no commit
	// yet, or that is on none, gets a row of its own.
	for _, w := range wts {
		i, ok := byName[w.branch]
		if !ok || w.branch == "" {
			b := Branch{Name: w.branch, Unborn: !w.detached, Detached: w.detached}
			if w.detached {
				b.Name = "(detached)"
				if len(w.head) >= 7 {
					b.Head = w.head[:7]
				}
				if baseRef != "" {
					b.Ahead, b.Behind = counts(r.Root, w.head, baseRef)
				}
				if s, err := git(r.Root, "log", "-1", "--format=%ct%x00%s", w.head); err == nil {
					when, subj, _ := strings.Cut(s, "\x00")
					b.When, _ = strconv.ParseInt(when, 10, 64)
					b.Subject = subj
				}
			}
			i = len(r.Branches)
			r.Branches = append(r.Branches, b)
		}
		r.Branches[i].Worktree = w.path
		if _, err := os.Stat(w.path); w.prunable || err != nil {
			r.Branches[i].Missing = true
		}
	}

	var wg sync.WaitGroup
	for i := range r.Branches {
		if b := &r.Branches[i]; b.Worktree != "" && !b.Missing {
			wg.Add(1)
			go func() { defer wg.Done(); b.status() }()
		}
	}
	wg.Wait()

	for _, w := range who {
		if i := r.owner(w.Dir); i >= 0 {
			r.Branches[i].Agents = append(r.Branches[i].Agents, w.Name)
		}
	}

	sort.SliceStable(r.Branches, func(i, j int) bool {
		a, b := r.Branches[i], r.Branches[j]
		if a.IsBase != b.IsBase {
			return a.IsBase
		}
		if a.When != b.When {
			return a.When > b.When
		}
		return a.Name < b.Name
	})
	return r, nil
}

// owner is the branch whose worktree holds dir, the deepest one when one
// worktree sits inside another.
func (r Repo) owner(dir string) int {
	dir = filepath.Clean(dir)
	best, n := -1, 0
	for i, b := range r.Branches {
		wt := filepath.Clean(b.Worktree)
		if b.Worktree != "" && (dir == wt || strings.HasPrefix(dir, wt+"/")) && len(wt) > n {
			best, n = i, len(wt)
		}
	}
	return best
}

var statRe = regexp.MustCompile(`(\d+) (insertion|deletion)`)

// status counts what is not committed in the branch's worktree.
func (b *Branch) status() {
	out, err := git(b.Worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return
	}
	f := strings.Split(out, "\x00")
	for i := 0; i < len(f); i++ {
		e := f[i]
		if len(e) < 3 {
			continue
		}
		if e[:2] == "??" {
			b.New++
			continue
		}
		b.Changed++
		if e[0] == 'R' || e[0] == 'C' { // the next field is the old path
			i++
		}
	}
	if b.Unborn || b.Changed == 0 {
		return
	}
	out, _ = git(b.Worktree, "diff", "--shortstat", "HEAD")
	for _, m := range statRe.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		if m[2] == "insertion" {
			b.Added = n
		} else {
			b.Deleted = n
		}
	}
}

// Level says how a note reads: plain, nothing left to do, or something to do.
type Level int

const (
	Plain Level = iota
	Good
	Todo
)

// VsBase is how the branch stands against the base branch.
func (r Repo) VsBase(b Branch) (string, Level) {
	switch {
	case b.IsBase:
		return "base", Plain
	case b.Unborn:
		return "no commits", Plain
	case r.Base == "":
		return "", Plain
	case b.Ahead == 0 && b.Behind == 0:
		return "same as " + r.Base, Plain
	case b.Ahead == 0:
		return fmt.Sprintf("in %s, %d behind", r.Base, b.Behind), Good
	case b.Behind == 0:
		return fmt.Sprintf("%d ahead of %s", b.Ahead, r.Base), Todo
	}
	return fmt.Sprintf("%d ahead, %d behind %s", b.Ahead, b.Behind, r.Base), Todo
}

// Pushed is how the branch stands against the remote.
func (r Repo) Pushed(b Branch) (string, Level) {
	switch {
	case r.Remote == "":
		return "no remote", Plain
	case b.Unborn:
		return "", Plain
	case b.Gone:
		return "upstream gone", Todo
	case !b.OnRemote:
		return "not pushed", Todo
	case b.Unpushed > 0 && b.Unpulled > 0:
		return fmt.Sprintf("%d to push, %d to pull", b.Unpushed, b.Unpulled), Todo
	case b.Unpushed > 0:
		return fmt.Sprintf("%d to push", b.Unpushed), Todo
	case b.Unpulled > 0:
		return fmt.Sprintf("%d to pull", b.Unpulled), Plain
	}
	return "pushed", Good
}

// Uncommitted is what sits in the branch's worktree, uncommitted.
func (b Branch) Uncommitted() (string, Level) {
	switch {
	case b.Worktree == "":
		return "", Plain
	case b.Missing:
		return "worktree folder gone", Todo
	case b.Changed == 0 && b.New == 0:
		return "clean", Good
	}
	var parts []string
	if b.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", b.Changed))
	}
	if b.New > 0 {
		parts = append(parts, fmt.Sprintf("%d new", b.New))
	}
	if b.Added > 0 || b.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("+%d -%d", b.Added, b.Deleted))
	}
	return strings.Join(parts, ", "), Todo
}

// Age is how long ago a unix time was, in its largest unit.
func Age(t, now int64) string {
	d := max(0, now-t)
	switch {
	case t == 0:
		return ""
	case d < 60:
		return "now"
	case d < 3600:
		return fmt.Sprintf("%dm", d/60)
	case d < 86400:
		return fmt.Sprintf("%dh", d/3600)
	}
	return fmt.Sprintf("%dd", d/86400)
}

// Short is a remote URL without the scheme, the user and the .git suffix.
func Short(url string) string {
	_, rest, scheme := strings.Cut(url, "://")
	if !scheme {
		rest = url
	}
	if at := strings.Index(rest, "@"); at >= 0 && at < strings.IndexAny(rest+"/", "/") {
		rest = rest[at+1:]
	}
	if host, path, ok := strings.Cut(rest, ":"); ok && !scheme && !strings.Contains(host, "/") {
		rest = host + "/" + path // scp style: git@host:me/repo
	}
	return strings.TrimSuffix(rest, ".git")
}
