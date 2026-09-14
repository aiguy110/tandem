// Package assets stores validated prompt images in Tandem's content-addressed
// on-disk layout and records per-agent authorization in SQLite.
package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aiguy110/tandem/internal/store"
)

const (
	MaxAssetBytes       = 10 * 1024 * 1024
	MaxPromptImages     = 4
	MaxPromptImageBytes = 20 * 1024 * 1024
	MaxImagePixels      = 40_000_000
	MaxImageDimension   = 16_384
)

var validID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type UnsupportedError struct{ Message string }

func (e *UnsupportedError) Error() string { return e.Message }

type TooLargeError struct{ Limit int64 }

func (e *TooLargeError) Error() string { return fmt.Sprintf("image exceeds %d byte limit", e.Limit) }

var ErrNotFound = errors.New("asset not found")
var ErrCorrupt = errors.New("asset data is corrupt")

type Stored struct {
	AssetID  string `json:"assetId"`
	MIMEType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Data     []byte `json:"-"`
}

type MetadataStore interface {
	PutAsset(sessionID string, asset store.Asset) error
	SessionAsset(sessionID, assetID string) (*store.Asset, error)
}

type Store struct {
	root string
	meta MetadataStore
}

func Open(root string, meta MetadataStore) (*Store, error) {
	if meta == nil {
		return nil, errors.New("asset metadata store is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create asset root: %w", err)
	}
	return &Store{root: root, meta: meta}, nil
}

func (s *Store) Put(sessionID string, data []byte, declaredMIME string) (Stored, error) {
	if int64(len(data)) > MaxAssetBytes {
		return Stored{}, &TooLargeError{Limit: MaxAssetBytes}
	}
	mime, width, height, err := inspect(data)
	if err != nil {
		return Stored{}, err
	}
	if width < 1 || height < 1 {
		return Stored{}, &UnsupportedError{Message: "image header is malformed or incomplete"}
	}
	if width > MaxImageDimension || height > MaxImageDimension || int64(width)*int64(height) > MaxImagePixels {
		return Stored{}, &UnsupportedError{Message: fmt.Sprintf("image dimensions exceed the %d pixel safety limit", MaxImagePixels)}
	}
	declared := strings.ToLower(strings.TrimSpace(strings.SplitN(declaredMIME, ";", 2)[0]))
	if declared != "" && declared != "application/octet-stream" && declared != mime {
		return Stored{}, &UnsupportedError{Message: fmt.Sprintf("content type %s does not match %s bytes", declared, mime)}
	}
	digest := sha256.Sum256(data)
	id := hex.EncodeToString(digest[:])
	dir := filepath.Join(s.root, id[:2])
	file := filepath.Join(dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Stored{}, err
	}
	// Publish the complete object with an atomic hard-link. A concurrent writer
	// of the same digest either wins the link or observes the already-complete
	// object; readers can never see a partially written asset.
	tmp, err := os.CreateTemp(dir, id+".tmp-")
	if err != nil {
		return Stored{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Stored{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Stored{}, err
	}
	if err := tmp.Close(); err != nil {
		return Stored{}, err
	}
	if err := os.Link(tmpName, file); err != nil && !errors.Is(err, fs.ErrExist) {
		return Stored{}, err
	}
	asset := store.Asset{ID: id, MIMEType: mime, Size: int64(len(data))}
	if err := s.meta.PutAsset(sessionID, asset); err != nil {
		return Stored{}, err
	}
	return Stored{AssetID: id, MIMEType: mime, Size: int64(len(data))}, nil
}

func (s *Store) Get(sessionID, assetID string) (Stored, error) {
	if !validID.MatchString(assetID) {
		return Stored{}, ErrNotFound
	}
	meta, err := s.meta.SessionAsset(sessionID, assetID)
	if err != nil {
		return Stored{}, err
	}
	if meta == nil {
		return Stored{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.root, assetID[:2], assetID))
	if errors.Is(err, fs.ErrNotExist) {
		return Stored{}, ErrNotFound
	}
	if err != nil {
		return Stored{}, err
	}
	if int64(len(data)) != meta.Size {
		return Stored{}, ErrCorrupt
	}
	return Stored{AssetID: assetID, MIMEType: meta.MIMEType, Size: meta.Size, Data: data}, nil
}

func inspect(data []byte) (mime string, width, height int, err error) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		mime = "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		mime = "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		mime = "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		mime = "image/webp"
	default:
		return "", 0, 0, &UnsupportedError{Message: "only PNG, JPEG, GIF, and WebP images are supported"}
	}
	if mime == "image/webp" {
		width, height, err = webPDimensions(data)
	} else {
		var cfg image.Config
		cfg, _, err = image.DecodeConfig(bytes.NewReader(data))
		width, height = cfg.Width, cfg.Height
	}
	if err != nil {
		return "", 0, 0, &UnsupportedError{Message: "image header is malformed or incomplete"}
	}
	return mime, width, height, nil
}

func webPDimensions(data []byte) (int, int, error) {
	if len(data) < 16 {
		return 0, 0, fs.ErrInvalid
	}
	switch string(data[12:16]) {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, fs.ErrInvalid
		}
		return 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16,
			1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16, nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, fs.ErrInvalid
		}
		b0, b1, b2, b3 := data[21], data[22], data[23], data[24]
		return 1 + int(b0) + int(b1&0x3f)<<8, 1 + int(b1>>6) + int(b2)<<2 + int(b3&0x0f)<<10, nil
	case "VP8 ":
		if len(data) < 30 || !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, fs.ErrInvalid
		}
		return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff), nil
	default:
		return 0, 0, fs.ErrInvalid
	}
}
