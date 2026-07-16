package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aiguy110/tandem/internal/store"
)

func testStore(t *testing.T) (*Store, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assets, err := Open(filepath.Join(dir, "assets"), db)
	if err != nil {
		t.Fatal(err)
	}
	return assets, db, filepath.Join(dir, "assets")
}

func encoded(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&out, img)
	case "jpeg":
		err = jpeg.Encode(&out, img, nil)
	case "gif":
		err = gif.Encode(&out, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPutGetMIMESignaturesAndDeclaredType(t *testing.T) {
	s, _, _ := testStore(t)
	for _, tc := range []struct{ format, mime string }{{"png", "image/png"}, {"jpeg", "image/jpeg"}, {"gif", "image/gif"}} {
		t.Run(tc.format, func(t *testing.T) {
			data := encoded(t, tc.format)
			got, err := s.Put("api-1", data, tc.mime+"; charset=binary")
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := s.Get("api-1", got.AssetID)
			if err != nil || loaded.MIMEType != tc.mime || !bytes.Equal(loaded.Data, data) {
				t.Fatalf("loaded=%#v err=%v", loaded, err)
			}
		})
	}
	if _, err := s.Put("api-1", encoded(t, "png"), "image/jpeg"); err == nil {
		t.Fatal("accepted declared MIME that disagrees with signature")
	}
}

func TestMalformedSizeAndDimensionLimits(t *testing.T) {
	s, _, _ := testStore(t)
	for _, malformed := range [][]byte{[]byte("<svg/>"), {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}} {
		if _, err := s.Put("api-1", malformed, ""); err == nil {
			t.Fatalf("accepted malformed image %x", malformed)
		}
	}
	if _, err := s.Put("api-1", make([]byte, MaxAssetBytes+1), ""); !new(TooLargeError).matches(err) {
		t.Fatalf("oversize error=%T %v", err, err)
	}
	// VP8X carries decoded canvas dimensions in three-byte little-endian fields.
	webp := make([]byte, 30)
	copy(webp[:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8X")
	webp[24], webp[25] = 0, 64 // 16385 pixels after the encoded minus-one rule.
	if _, err := s.Put("api-1", webp, "image/webp"); err == nil {
		t.Fatal("accepted unsafe image dimensions")
	}
}

func (e *TooLargeError) matches(err error) bool {
	var target *TooLargeError
	return errors.As(err, &target)
}

func TestDeduplicationAuthorizationAndExistingLayout(t *testing.T) {
	s, db, root := testStore(t)
	data := encoded(t, "png")
	first, err := s.Put("api-1", data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put("api-2", data, "application/octet-stream")
	if err != nil || first.AssetID != second.AssetID {
		t.Fatalf("dedup IDs %q/%q err=%v", first.AssetID, second.AssetID, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, first.AssetID[:2]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("physical files=%d err=%v", len(entries), err)
	}
	if _, err := s.Get("api-3", first.AssetID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-agent get error=%v", err)
	}

	// Compatibility: a file and association written in the historical Node
	// layout remain readable without rewriting the physical object.
	legacy := encoded(t, "gif")
	digest := sha256.Sum256(legacy)
	id := hex.EncodeToString(digest[:])
	if err := os.MkdirAll(filepath.Join(root, id[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, id[:2], id), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.PutAsset("legacy-1", store.Asset{ID: id, MIMEType: "image/gif", Size: int64(len(legacy))}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("legacy-1", id)
	if err != nil || !bytes.Equal(got.Data, legacy) {
		t.Fatalf("legacy asset=%#v err=%v", got, err)
	}
}

func TestConcurrentDedupNeverPublishesPartialData(t *testing.T) {
	s, _, root := testStore(t)
	data := append(encoded(t, "png"), bytes.Repeat([]byte{0xab}, 2*1024*1024)...)
	const writers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	ids := make(chan string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored, err := s.Put("api-1", data, "image/png")
			if err == nil {
				ids <- stored.AssetID
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	close(ids)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for got := range ids {
		if id == "" {
			id = got
		} else if got != id {
			t.Fatalf("digest mismatch %q/%q", id, got)
		}
	}
	physical, err := os.ReadFile(filepath.Join(root, id[:2], id))
	if err != nil || !bytes.Equal(physical, data) {
		t.Fatalf("published bytes=%d err=%v", len(physical), err)
	}
	entries, err := os.ReadDir(filepath.Join(root, id[:2]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("dedup left %d files err=%v", len(entries), err)
	}
}

func TestWebPDimensionVariants(t *testing.T) {
	for _, kind := range []string{"VP8X", "VP8L", "VP8 "} {
		data := make([]byte, 30)
		copy(data[:4], "RIFF")
		copy(data[8:12], "WEBP")
		copy(data[12:16], kind)
		switch kind {
		case "VP8X":
			data[24], data[27] = 1, 2
		case "VP8L":
			data[20] = 0x2f
			data[21], data[22] = 1, 2
		case "VP8 ":
			copy(data[23:26], []byte{0x9d, 0x01, 0x2a})
			binary.LittleEndian.PutUint16(data[26:28], 2)
			binary.LittleEndian.PutUint16(data[28:30], 3)
		}
		mime, width, height, err := inspect(data)
		if err != nil || mime != "image/webp" || width < 1 || height < 1 {
			t.Errorf("%s: %s %dx%d err=%v", kind, mime, width, height, err)
		}
	}
}
