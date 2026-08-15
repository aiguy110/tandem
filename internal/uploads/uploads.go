// Package uploads writes browser-selected files into an agent's workspace.
package uploads

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/aiguy110/tandem/internal/workspacefs"
)

const SettingsPath = ".tandem/settings.json"

type settings struct {
	Uploads struct {
		Directory string `json:"directory"`
	} `json:"uploads"`
}

// Save writes data beneath the agent workspace. The upload directory defaults
// to the repository root and can be changed with
// {"uploads":{"directory":"relative/path"}} in .tandem/settings.json.
func Save(workspace, name string, data []byte) (string, error) {
	dir, err := directory(workspace)
	if err != nil {
		return "", err
	}
	name = safeName(name)
	fsys, err := workspacefs.Open(workspace)
	if err != nil {
		return "", err
	}
	defer fsys.Close()
	for n := 0; n < 10_000; n++ {
		candidate := uniqueName(name, n)
		rel := filepath.Join(dir, candidate)
		if err := fsys.WriteFile(rel, data, true); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("write uploaded file: %w", err)
		}
		return filepath.ToSlash(rel), nil
	}
	return "", errors.New("could not find an unused upload filename")
}

func directory(workspace string) (string, error) {
	data, err := workspacefs.Open(workspace)
	if err != nil {
		return "", err
	}
	defer data.Close()
	text, err := data.ReadTextFile(SettingsPath, nil, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ".", nil
		}
		return "", fmt.Errorf("read %s: %w", SettingsPath, err)
	}
	var config settings
	if err := json.Unmarshal([]byte(text), &config); err != nil {
		return "", fmt.Errorf("parse %s: %w", SettingsPath, err)
	}
	dir := strings.TrimSpace(config.Uploads.Directory)
	if dir == "" {
		return ".", nil
	}
	if filepath.IsAbs(dir) {
		return "", errors.New("upload directory must be repository-relative")
	}
	dir = filepath.Clean(dir)
	if dir == ".." || strings.HasPrefix(dir, ".."+string(filepath.Separator)) {
		return "", errors.New("upload directory escapes repository")
	}
	return dir, nil
}

func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." {
		return "upload"
	}
	return name
}

func uniqueName(name string, n int) string {
	if n == 0 {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (%d)%s", base, n, ext)
}
