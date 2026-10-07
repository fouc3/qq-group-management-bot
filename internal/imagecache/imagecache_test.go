package imagecache

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCache returns a cache in a directory of the test's own.
func testCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cache, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache
}

// pngPicture encodes a picture of the given size, with the pixel noise that keeps
// a PNG from compressing itself down to nothing.
func pngPicture(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	seed := uint32(12345)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// A cheap deterministic noise: the point is that neighbouring pixels
			// disagree, so the encoder cannot compress them away.
			seed = seed*1664525 + 1013904223
			picture.Set(x, y, color.RGBA{
				R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255,
			})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		t.Fatalf("encoding the test picture: %v", err)
	}
	return out.Bytes()
}

// pictureServer serves one body at every path.
func pictureServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			http.Error(w, "no longer here", http.StatusNotFound)
		default:
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestASmallPictureIsKeptAsItIs covers the ordinary case: nothing to shrink, and
// what comes back is the very same bytes the platform delivered.
func TestASmallPictureIsKeptAsItIs(t *testing.T) {
	body := pngPicture(t, 40, 30)
	server := pictureServer(t, body)
	cache := testCache(t, Config{Retention: time.Hour, CompressAboveBytes: 1 << 20})

	refs := cache.FetchAll(context.Background(), "MSG-1", []string{server.URL + "/a.png"})
	if len(refs) != 1 {
		t.Fatalf("kept %d pictures, want 1", len(refs))
	}
	stored, err := cache.Read(refs[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(stored, body) {
		t.Error("a picture under the compression threshold was altered")
	}
	if got := cache.MIME(refs[0]); got != mimePNG {
		t.Errorf("MIME = %q, want %q", got, mimePNG)
	}
}

// TestAnOversizedPictureIsShrunkBeforeItIsKept covers the compression switch: the
// picture that arrives is over the threshold, and what is stored is a smaller copy.
func TestAnOversizedPictureIsShrunkBeforeItIsKept(t *testing.T) {
	body := pngPicture(t, 300, 300)
	if int64(len(body)) < 1024 {
		t.Fatalf("the test picture is %d bytes, too small to prove anything", len(body))
	}
	server := pictureServer(t, body)
	cache := testCache(t, Config{
		Retention:          time.Hour,
		CompressAboveBytes: 1024,
		MaxBytes:           1 << 20,
	})

	refs := cache.FetchAll(context.Background(), "MSG-2", []string{server.URL + "/big.png"})
	if len(refs) != 1 {
		t.Fatalf("kept %d pictures, want 1", len(refs))
	}
	stored, err := cache.Read(refs[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(stored) >= len(body) {
		t.Errorf("the stored picture is %d bytes and the delivered one %d: it was "+
			"supposed to be shrunk", len(stored), len(body))
	}
	if mime := sniff(stored); mime != mimeJPEG {
		t.Errorf("the shrunk picture is stored as %q, want a JPEG", mime)
	}
	if !strings.HasSuffix(refs[0], ".jpg") {
		t.Errorf("the shrunk picture is stored as %q, want a .jpg", refs[0])
	}
	// And it is a picture that decodes: a name that reads back is not the point.
	if _, err := jpeg.Decode(bytes.NewReader(stored)); err != nil {
		t.Errorf("the shrunk picture does not decode: %v", err)
	}
}

// TestAPictureOverTheLimitIsNotKept covers the second threshold: what shrinking
// cannot bring under the limit is not stored at all.
func TestAPictureOverTheLimitIsNotKept(t *testing.T) {
	body := pngPicture(t, 300, 300)
	server := pictureServer(t, body)
	cache := testCache(t, Config{
		Retention: time.Hour,
		// No compression at all, so the only thing that can happen is the drop.
		MaxBytes: 512,
	})

	refs := cache.FetchAll(context.Background(), "MSG-3", []string{server.URL + "/big.png"})
	if len(refs) != 0 {
		t.Fatalf("kept %v, want nothing: the picture is over the limit", refs)
	}
	entries, err := os.ReadDir(cache.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %d entries, want nothing left over", len(entries))
	}
}

// TestSomethingThatIsNotAPictureIsRefused covers the URL that answers with
// something else: an error page behind a 200, a login wall, a redirect home.
func TestSomethingThatIsNotAPictureIsRefused(t *testing.T) {
	server := pictureServer(t, []byte("<html><body>登录后可见</body></html>"))
	cache := testCache(t, Config{Retention: time.Hour})

	if refs := cache.FetchAll(context.Background(), "MSG-4",
		[]string{server.URL + "/login"}); len(refs) != 0 {
		t.Fatalf("kept %v, want nothing: what arrived is not a picture", refs)
	}
}

// TestAnAnimatedGIFIsKeptWhole covers the one picture that must not be shrunk:
// re-encoding it would keep the first frame and throw the movement away.
func TestAnAnimatedGIFIsKeptWhole(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	frames := &gif.GIF{Image: make([]*image.Paletted, 2), Delay: []int{10, 10}}
	for index := range frames.Image {
		frame := image.NewPaletted(image.Rect(0, 0, 60, 60), palette)
		for y := 0; y < 60; y++ {
			for x := 0; x < 60; x++ {
				frame.SetColorIndex(x, y, uint8((x+y+index)%2))
			}
		}
		frames.Image[index] = frame
	}
	var body bytes.Buffer
	if err := gif.EncodeAll(&body, frames); err != nil {
		t.Fatalf("encoding the test GIF: %v", err)
	}
	server := pictureServer(t, body.Bytes())
	cache := testCache(t, Config{Retention: time.Hour, CompressAboveBytes: 1})

	refs := cache.FetchAll(context.Background(), "MSG-5", []string{server.URL + "/a.gif"})
	if len(refs) != 1 {
		t.Fatalf("kept %d pictures, want 1", len(refs))
	}
	stored, err := cache.Read(refs[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(stored, body.Bytes()) {
		t.Error("an animated GIF was re-encoded, so it no longer moves")
	}
}

// TestOneUnreachablePictureDoesNotCostTheOthers covers the delivery that is partly
// gone: a picture that cannot be fetched is not a message without pictures.
func TestOneUnreachablePictureDoesNotCostTheOthers(t *testing.T) {
	server := pictureServer(t, pngPicture(t, 20, 20))
	cache := testCache(t, Config{Retention: time.Hour})

	refs := cache.FetchAll(context.Background(), "MSG-6", []string{
		server.URL + "/gone",
		server.URL + "/a.png",
	})
	if len(refs) != 1 {
		t.Fatalf("kept %d pictures, want the one that was still there", len(refs))
	}
}

// TestTheSameMessageAndPositionAlwaysLandsOnTheSameName covers what makes the
// double delivery of a mention harmless: the same picture is written to the same
// name, so the second delivery is the same write.
func TestTheSameMessageAndPositionAlwaysLandsOnTheSameName(t *testing.T) {
	server := pictureServer(t, pngPicture(t, 20, 20))
	cache := testCache(t, Config{Retention: time.Hour})

	first := cache.FetchAll(context.Background(), "MSG-7", []string{server.URL + "/a.png"})
	second := cache.FetchAll(context.Background(), "MSG-7", []string{server.URL + "/a.png"})
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("the same message and position gave %v and %v", first, second)
	}
}

// TestOnlySoManyPicturesOfOneMessageAreKept covers the cap: a message with a
// hundred attachments is not a hundred downloads.
func TestOnlySoManyPicturesOfOneMessageAreKept(t *testing.T) {
	server := pictureServer(t, pngPicture(t, 20, 20))
	cache := testCache(t, Config{Retention: time.Hour, MaxPerMessage: 2})

	refs := cache.FetchAll(context.Background(), "MSG-8", []string{
		server.URL + "/1.png", server.URL + "/2.png", server.URL + "/3.png",
	})
	if len(refs) != 2 {
		t.Fatalf("kept %d pictures, want the configured 2", len(refs))
	}
}

// TestAPictureIsOnlyReadByTheNameTheCacheWrote covers the reference that came back
// from Redis: a name in a shared cache is not trusted to stay in the directory.
func TestAPictureIsOnlyReadByTheNameTheCacheWrote(t *testing.T) {
	cache := testCache(t, Config{Retention: time.Hour})
	for _, ref := range []string{"../outside.jpg", "sub/picture.jpg", "/etc/passwd",
		"..", ".hidden.jpg", ""} {
		if _, err := cache.Read(ref); err == nil {
			t.Errorf("Read(%q) was allowed", ref)
		}
	}
}

// TestPicturesAreSweptOnceTheirMessagesHaveAgedOut covers the half of the cleanup
// that is not the message cache's to do.
func TestPicturesAreSweptOnceTheirMessagesHaveAgedOut(t *testing.T) {
	server := pictureServer(t, pngPicture(t, 20, 20))
	cache := testCache(t, Config{Retention: time.Hour, DownloadTimeout: time.Second})

	refs := cache.FetchAll(context.Background(), "MSG-9", []string{server.URL + "/a.png"})
	if len(refs) != 1 {
		t.Fatalf("kept %d pictures, want 1", len(refs))
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(cache.Dir(), refs[0]), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	cache.Sweep(context.Background())

	entries, err := os.ReadDir(cache.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %d entries, want the aged-out picture removed",
			len(entries))
	}
}

// TestAPictureStillInsideItsRetentionIsLeftAlone is the other half of the sweep:
// it removes what has aged out and nothing else.
func TestAPictureStillInsideItsRetentionIsLeftAlone(t *testing.T) {
	server := pictureServer(t, pngPicture(t, 20, 20))
	cache := testCache(t, Config{Retention: time.Hour, DownloadTimeout: time.Second})

	refs := cache.FetchAll(context.Background(), "MSG-10", []string{server.URL + "/a.png"})
	cache.Sweep(context.Background())
	if _, err := cache.Read(refs[0]); err != nil {
		t.Errorf("a picture inside its retention was swept: %v", err)
	}
}
