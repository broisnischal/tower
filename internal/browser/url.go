package browser

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// searchURL is where words that are not an address go.
const searchURL = "https://www.google.com/search?q="

var (
	hasScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	// Schemes written without slashes.
	bareSchemes = []string{"about:", "data:", "file:", "chrome:", "view-source:", "javascript:", "blob:"}
	// host[:port][/rest] where host has a dot and a letter TLD.
	domain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*\.[a-zA-Z]{2,}(:\d+)?([/?#].*)?$`)
	// localhost, IPv4 or [IPv6], with an optional port and path.
	local = regexp.MustCompile(`^(localhost|\d{1,3}(\.\d{1,3}){3}|\[[0-9a-fA-F:]+\])(:\d+)?([/?#].*)?$`)
	port  = regexp.MustCompile(`^:\d+([/?#].*)?$`)
)

// Resolve turns what I type in the address bar into a URL: full URLs stay,
// hosts get a scheme (http for local ones, https otherwise), ":3000" means
// localhost, absolute paths become file URLs, and anything else is a
// search.
func Resolve(in string) string {
	s := strings.TrimSpace(in)
	switch {
	case s == "":
		return ""
	case hasScheme.MatchString(s):
		return s
	case hasBareScheme(s):
		return s
	case strings.ContainsAny(s, " \t"):
		return search(s)
	case port.MatchString(s):
		return "http://localhost" + s
	case local.MatchString(s):
		return "http://" + s
	case domain.MatchString(s):
		return "https://" + s
	case strings.HasPrefix(s, "/"):
		return "file://" + s
	}
	return search(s)
}

func hasBareScheme(s string) bool {
	l := strings.ToLower(s)
	for _, p := range bareSchemes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func search(q string) string { return searchURL + url.QueryEscape(q) }

// ResolveArg is Resolve for a command line argument: a file that exists
// relative to where I ran tower opens as a file URL.
func ResolveArg(arg string) string {
	if arg == "" {
		return ""
	}
	if !hasScheme.MatchString(arg) && !local.MatchString(arg) && !port.MatchString(arg) {
		p := arg
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return "file://" + abs
			}
		}
	}
	return Resolve(arg)
}

// Default is the dev server on localhost:3000 when something listens
// there, else a blank page.
func Default() string {
	c, err := net.DialTimeout("tcp", "127.0.0.1:3000", 200*time.Millisecond)
	if err != nil {
		return "about:blank"
	}
	c.Close()
	return "http://localhost:3000"
}

// Host is the short name of a URL for a window title: host and port, the
// file name for file URLs, else the scheme's opaque part (about:blank
// gives "blank").
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch {
	case u.Host != "":
		return u.Host
	case u.Scheme == "file":
		return filepath.Base(u.Path)
	case u.Opaque != "":
		return u.Opaque
	}
	return raw
}
