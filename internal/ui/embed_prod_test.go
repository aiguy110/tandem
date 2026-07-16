//go:build !tandem_dev

package ui

import (
	"io/fs"
	"testing"
)

func TestProductionBuildContainsReactApplication(t *testing.T) {
	files, ok := Filesystem()
	if !ok {
		t.Fatal("production UI filesystem is unavailable; run scripts/stage-go-ui.sh before building")
	}
	for _, name := range []string{"index.html", "ghostty-vt.wasm"} {
		info, err := fs.Stat(files, name)
		if err != nil || info.IsDir() || info.Size() == 0 {
			t.Fatalf("embedded %s info=%v err=%v", name, info, err)
		}
	}
}
