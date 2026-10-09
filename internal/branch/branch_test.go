package branch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScan builds a repo with a remote and one branch in each state I care
// about, then checks what Scan says about each.
func TestScan(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	origin, repo := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "repo")
	run(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	run(t, tmp, "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "a.txt"), "one\n")
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "first")
	run(t, repo, "remote", "add", "origin", origin)
	run(t, repo, "push", "-q", "-u", "origin", "main")
	run(t, repo, "remote", "set-head", "origin", "main")

	// feature: two commits of its own, one pushed, plus uncommitted work.
	feat := filepath.Join(tmp, "repo-feat")
	run(t, repo, "worktree", "add", "-q", "-b", "tower/feat", feat)
	write(t, filepath.Join(feat, "b.txt"), "b\n")
	run(t, feat, "add", ".")
	run(t, feat, "commit", "-q", "-m", "add b")
	run(t, feat, "push", "-q", "-u", "origin", "tower/feat")
	write(t, filepath.Join(feat, "c.txt"), "c\n")
	run(t, feat, "add", ".")
	run(t, feat, "commit", "-q", "-m", "add c")
	write(t, filepath.Join(feat, "a.txt"), "one\ntwo\nthree\n")
	write(t, filepath.Join(feat, "new.txt"), "new\n")
	os.MkdirAll(filepath.Join(feat, "dir"), 0o755)
	write(t, filepath.Join(feat, "dir", "x.txt"), "x\n")

	// merged: its commit is in main, and main moved on.
	run(t, repo, "branch", "merged")
	write(t, filepath.Join(repo, "a.txt"), "one\nmain\n")
	run(t, repo, "commit", "-q", "-am", "main moves")
	run(t, repo, "push", "-q")

	// empty: a worktree made before its branch has a commit.
	empty := filepath.Join(tmp, "repo-empty")
	run(t, repo, "worktree", "add", "-q", "--orphan", "-b", "tower/empty", empty)

	r, err := Scan(feat, []Who{{"me", repo}, {"helper", filepath.Join(feat, "dir")}, {"stranger", tmp}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != repo || r.Base != "main" || r.Remote != "origin" {
		t.Fatalf("repo = %+v", r)
	}
	got := map[string]Branch{}
	var order []string
	for _, b := range r.Branches {
		got[b.Name] = b
		order = append(order, b.Name)
	}
	if order[0] != "main" || order[len(order)-1] != "tower/empty" {
		t.Errorf("order = %v, want main first and the empty worktree last", order)
	}

	check := func(name string, want ...string) {
		t.Helper()
		b, ok := got[name]
		if !ok {
			t.Fatalf("no branch %s in %v", name, order)
		}
		vs, _ := r.VsBase(b)
		push, _ := r.Pushed(b)
		dirty, _ := b.Uncommitted()
		have := []string{vs, push, dirty, strings.Join(b.Agents, ",")}
		if strings.Join(have, " | ") != strings.Join(want, " | ") {
			t.Errorf("%s: got %q, want %q", name, have, want)
		}
	}
	check("main", "base", "pushed", "clean", "me")
	check("tower/feat", "2 ahead, 1 behind main", "1 to push", "1 changed, 2 new, +2 -0", "helper")
	check("merged", "in main, 1 behind", "not pushed", "", "")
	check("tower/empty", "no commits", "", "clean", "")
	if got["tower/feat"].Worktree != feat || !got["tower/empty"].Unborn {
		t.Errorf("worktrees: %+v", r.Branches)
	}

	os.RemoveAll(empty)
	r, _ = Scan(repo, nil)
	for _, b := range r.Branches {
		if b.Name == "tower/empty" {
			if s, _ := b.Uncommitted(); s != "worktree folder gone" {
				t.Errorf("removed worktree: %q", s)
			}
		}
	}

	if _, err := Scan(tmp, nil); err == nil {
		t.Error("Scan outside a repo did not fail")
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/me/tower.git":     "github.com/me/tower",
		"git@github.com:me/tower.git":         "github.com/me/tower",
		"ssh://git@example.com:2222/me/tower": "example.com:2222/me/tower",
		"https://user@gitlab.com/group/sub/x": "gitlab.com/group/sub/x",
		"/srv/git/tower.git":                  "/srv/git/tower",
	} {
		if got := Short(in); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAge(t *testing.T) {
	for _, c := range []struct {
		d    int64
		want string
	}{{5, "now"}, {125, "2m"}, {7300, "2h"}, {200000, "2d"}} {
		if got := Age(1_000_000-c.d, 1_000_000); got != c.want {
			t.Errorf("Age(-%d) = %q, want %q", c.d, got, c.want)
		}
	}
}
