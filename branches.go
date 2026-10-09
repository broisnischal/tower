package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"tower/internal/agent"
	"tower/internal/branch"
	"tower/internal/thread"
)

// branches lists every branch of every repo my agents work in, or of the
// folders given: where each is checked out and who works there, how it
// stands against the base branch and the remote, and what is uncommitted.
func branches(args []string) error {
	fs := flag.NewFlagSet("tower branches", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fetch := fs.Bool("fetch", false, "fetch each repo's remote first, so the push state is current")
	if err := fs.Parse(args); err != nil {
		return err
	}
	who := workers()
	dirs := fs.Args()
	if len(dirs) == 0 {
		for _, w := range who {
			dirs = append(dirs, w.Dir)
		}
		if d, err := os.Getwd(); err == nil {
			dirs = append(dirs, d)
		}
	}

	var roots, notGit []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d, _ = filepath.Abs(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		c, ok := branch.Common(d)
		switch {
		case !ok:
			notGit = append(notGit, d)
		case !seen[c]:
			seen[c] = true
			roots = append(roots, d)
		}
	}

	repos := make([]branch.Repo, len(roots))
	errs := make([]error, len(roots))
	var wg sync.WaitGroup
	for i, d := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if *fetch {
				if err := branch.Fetch(d); err != nil {
					fmt.Fprintf(os.Stderr, "tower: fetch in %s: %v\n", tilde(d), err)
				}
			}
			repos[i], errs[i] = branch.Scan(d, who)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Root < repos[j].Root })

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Repos  []branch.Repo `json:"repos"`
			NotGit []string      `json:"not_git,omitempty"`
		}{repos, notGit})
	}
	if len(repos) == 0 {
		fmt.Println("no git repos")
	}
	now := time.Now().Unix()
	for i, r := range repos {
		if i > 0 {
			fmt.Println()
		}
		info := []string{}
		if r.URL != "" {
			info = append(info, branch.Short(r.URL))
		}
		if r.Base != "" {
			info = append(info, "base "+r.Base)
		}
		if r.Remote != "" {
			if r.Fetched > 0 {
				ago := branch.Age(r.Fetched, now) + " ago"
				if ago == "now ago" {
					ago = "just now"
				}
				info = append(info, "last fetch "+ago)
			} else {
				info = append(info, "never fetched")
			}
		}
		fmt.Println(tilde(r.Root) + "  " + strings.Join(info, " · "))
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		vsBase := "VS BASE"
		if r.Base != "" {
			vsBase = "VS " + strings.ToUpper(r.Base)
		}
		fmt.Fprintf(tw, "  BRANCH\tWORKTREE\tAGENT\tHEAD\t%s\tREMOTE\tUNCOMMITTED\n", vsBase)
		for _, b := range r.Branches {
			vs, _ := r.VsBase(b)
			push, _ := r.Pushed(b)
			dirty, _ := b.Uncommitted()
			head := strings.TrimSpace(b.Head + " " + branch.Age(b.When, now))
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.Name, dash(tilde(b.Worktree)), dash(strings.Join(b.Agents, ",")),
				dash(head), dash(vs), dash(push), dash(dirty))
		}
		tw.Flush()
	}
	if len(notGit) > 0 {
		names := map[string][]string{}
		for _, w := range who {
			d, _ := filepath.Abs(w.Dir)
			names[d] = append(names[d], w.Name)
		}
		var parts []string
		for _, d := range notGit {
			s := tilde(d)
			if n := names[d]; len(n) > 0 {
				s += " (" + strings.Join(n, ", ") + ")"
			}
			parts = append(parts, s)
		}
		fmt.Println("\nnot in a git repo: " + strings.Join(parts, ", "))
	}
	return nil
}

// workers is every agent and headless thread, with the folder it works in.
func workers() []branch.Who {
	var who []branch.Who
	for _, a := range agent.Load(true) {
		if a.Cwd != "" {
			who = append(who, branch.Who{Name: a.Name, Dir: a.Cwd})
		}
	}
	if c, err := thread.Dial(false); err == nil {
		var ts []thread.Thread
		c.Call("list", nil, &ts)
		c.Close()
		for _, t := range ts {
			if t.Cwd != "" {
				who = append(who, branch.Who{Name: t.Name, Dir: t.Cwd})
			}
		}
	}
	return who
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tilde(p string) string {
	if h, err := os.UserHomeDir(); err == nil && p != "" && (p == h || strings.HasPrefix(p, h+"/")) {
		return "~" + p[len(h):]
	}
	return p
}
