//go:build windows

package ptyadapter

import (
	"context"
	"errors"
	"testing"

	processx "github.com/aiguy110/tandem/internal/process"
)

func TestUnsupportedPlatformFailsClearly(t *testing.T) {
	a := New("unsupported")
	err := a.Spawn(context.Background(), SpawnOptions{Command: "cmd.exe"})
	if !errors.Is(err, processx.ErrPTYUnsupported) {
		t.Fatalf("spawn error = %v, want ErrPTYUnsupported", err)
	}
}
