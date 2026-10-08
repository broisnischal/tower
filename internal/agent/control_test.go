package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitImagesPastesExistingImagesOnTheirOwn(t *testing.T) {
	img := filepath.Join(t.TempDir(), "shot.png")
	os.WriteFile(img, []byte("x"), 0o600)
	got := splitImages("compare " + img + " with /no/such/file.png please")
	want := []part{{text: "compare "}, {text: img, image: true}, {text: " with /no/such/file.png please"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("part %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
