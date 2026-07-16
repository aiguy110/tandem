//go:build windows

package process

import (
	"context"
	"os"
)

func StartPTY(context.Context, Spec, Size, Options) (*PTY, error) {
	return nil, ErrPTYUnsupported
}

func resizePTY(*os.File, Size) error        { return ErrPTYUnsupported }
func normalizePTYReadError(err error) error { return err }
