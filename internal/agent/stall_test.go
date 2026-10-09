package agent

import "testing"

func TestAPIWait(t *testing.T) {
	for line, want := range map[string]bool{
		"  ⎿  API Error (529 overloaded_error) · Retrying in 18s · attempt 4/10": true,
		"  ⎿  Request timed out · Retrying in 1m 4s · attempt 7/10":              true,
		"No response from the API after 60s":                                     true,
		"⏺ I'll retry the request in a moment":                                   false,
		"Retrying in 5 seconds":                                                  false,
	} {
		if got := apiWait.MatchString(line); got != want {
			t.Errorf("apiWait(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestDraftOf(t *testing.T) {
	rule := "──────────────────────────────"
	for screen, want := range map[string]string{
		"⏺ done\n" + rule + "\n❯ list three facts\n" + rule + "\n  ⏵⏵ auto mode on": "list three facts",
		"⏺ done\n" + rule + "\n❯ \n" + rule + "\n  ⏵⏵ auto mode on":                 "",
		rule + "\n❯ first line\n  second line\n" + rule + "\n":                      "first line\nsecond line",
		rule + "\n❯ Try \"fix lint errors\"\n" + rule + "\n":                        "",
		"no box on screen": "",
	} {
		if got := draftOf(screen); got != want {
			t.Errorf("draftOf(%q) = %q, want %q", screen, got, want)
		}
	}
}

func TestWaitLine(t *testing.T) {
	rule := "──────────────────────────────"
	banner := "  ⎿  API Error (529 overloaded_error) · Retrying in 18s · attempt 3/10"
	for screen, want := range map[string]bool{
		// the wait in the status lines above the box
		"⏺ Working\n✻ Pondering…\n" + banner + "\n\n" + rule + "\n❯ \n" + rule + "\n  ⏵⏵ auto": true,
		// the same words further up, in a command's output
		"⏺ Bash(cat fake.py)\n" + banner + "\n  ⎿  line\n  line\n  line\n✻ Pondering…\n" + rule + "\n❯ \n" + rule: false,
		// no input box on screen
		banner: false,
	} {
		if _, got := waitLine(screen); got != want {
			t.Errorf("waitLine(%q) = %v, want %v", screen, got, want)
		}
	}
}
