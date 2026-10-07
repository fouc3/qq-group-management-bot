// Package imagecache keeps the pictures members posted, on disk.
//
// Why it exists at all: the platform hands an attachment over exactly once. The
// URL in the event is a presigned link with a short life, and nothing in the API
// fetches a message back afterwards -- the SDK has no download method and no
// get-message endpoint either. A picture nobody downloaded while that event was in
// hand is a picture nobody will ever see again, which is why a judgement that is
// allowed to look at pictures has to have them fetched long before it runs.
//
// It is a cache and is treated as one: every operation may fail, nothing here is a
// source of truth, and losing the whole directory costs a judgement its pictures
// rather than costing it its answer. A picture that cannot be fetched is a
// judgement without pictures, never a report refused.
package imagecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	_ "image/png" // registered so that a PNG decodes when it has to be shrunk
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// hardCeiling is what one picture is read as when the configuration sets no
	// limit at all. It is not a policy, it is a guard: a URL is not a thing to read
	// without a bound, and this is far above any picture the platform delivers.
	hardCeiling = 32 << 20
	// sweepInterval is how often the directory is looked at while pictures are
	// arriving. Pruning costs a directory walk, and one per picture would cost more
	// than it saves.
	sweepInterval = 5 * time.Minute
	// jpegQuality is what a shrunk picture is written back with. High enough that
	// text baked into an advertisement stays readable, low enough that the file is
	// worth the shrinking.
	jpegQuality = 80
	// defaultMaxEdge is the longest side a shrunk picture is kept at: the size
	// models are usually fed at, above which they scale it down themselves anyway.
	defaultMaxEdge = 1568
	// refBytes is how much of the hash names a stored picture. Sixteen hex
	// characters: a collision needs more pictures than this cache will ever hold,
	// and the whole name stays short enough to sit in every cached message.
	refBytes = 8
)

// Config is how the cache is built.
type Config struct {
	// Dir is where pictures are written. Must be set.
	Dir string
	// Retention is how long a picture stays. It is the message cache's own
	// retention: a picture outliving the message it came with is a file nobody can
	// reach, and one that dies first is a judgement without the evidence.
	Retention time.Duration
	// DownloadTimeout bounds the whole of one message's pictures, not each of
	// them: the message path is held open for as long as this takes, and a per
	// picture budget multiplied by the number of pictures is a stall with a
	// misleading name.
	DownloadTimeout time.Duration
	// MaxPerMessage is how many pictures of one message are kept. Zero keeps every
	// one of them.
	MaxPerMessage int
	// CompressAboveBytes is the size over which a picture is shrunk before it is
	// stored. Zero turns compression off.
	CompressAboveBytes int64
	// MaxBytes is the size over which a picture is not stored at all, applied after
	// compression. Zero means no limit but the guard above.
	MaxBytes int64
	// MaxEdge is the longest side a shrunk picture is kept at. Zero uses the
	// default.
	MaxEdge int
	// Logger may be nil, in which case nothing is said about a picture that could
	// not be kept -- which is the ordinary case for a cache that is simply down.
	Logger *slog.Logger
}

// Cache is the picture directory.
type Cache struct {
	dir           string
	retention     time.Duration
	timeout       time.Duration
	maxPerMessage int
	compressAbove int64
	maxBytes      int64
	maxEdge       int
	log           *slog.Logger
	client        *http.Client

	mu        sync.Mutex
	lastSweep time.Time
}

// DefaultDir is where pictures are kept when the configuration names no place.
//
// Under the user's cache directory rather than the state directory the database
// lives in: this is something whose loss costs a judgement its pictures and costs
// nothing else, and putting it with the state would suggest otherwise.
func DefaultDir() (string, error) {
	directory := os.Getenv("XDG_CACHE_HOME")
	if strings.TrimSpace(directory) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the home directory: %w", err)
		}
		directory = filepath.Join(home, ".cache")
	}
	return filepath.Join(directory, "qq-group-management-bot", "images"), nil
}

// New prepares the directory and returns the cache.
//
// The directory is created here rather than at the first picture, so that a
// deployment with an unwritable place to keep pictures finds out at startup rather
// than on the first report.
func New(cfg Config) (*Cache, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return nil, errors.New("imagecache: no directory is configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the image cache directory %s: %w", dir, err)
	}
	timeout := cfg.DownloadTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	maxEdge := cfg.MaxEdge
	if maxEdge <= 0 {
		maxEdge = defaultMaxEdge
	}
	return &Cache{
		dir:           dir,
		retention:     cfg.Retention,
		timeout:       timeout,
		maxPerMessage: cfg.MaxPerMessage,
		compressAbove: cfg.CompressAboveBytes,
		maxBytes:      cfg.MaxBytes,
		maxEdge:       maxEdge,
		log:           cfg.Logger,
		client:        &http.Client{Timeout: timeout},
	}, nil
}

// Dir is where pictures are kept, for the log line that says so.
func (c *Cache) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// FetchAll downloads one message's pictures and returns what was kept.
//
// The order is the order they were posted in, and a picture that could not be
// fetched is simply not represented: what comes back is the list of pictures a
// judgement may look at, not a description of the message.
func (c *Cache) FetchAll(ctx context.Context, messageID string, urls []string) []string {
	if c == nil || len(urls) == 0 || strings.TrimSpace(messageID) == "" {
		return nil
	}
	if c.maxPerMessage > 0 && len(urls) > c.maxPerMessage {
		c.logDebug("a message carried more pictures than are kept",
			"message", messageID, "carried", len(urls), "kept", c.maxPerMessage)
		urls = urls[:c.maxPerMessage]
	}

	// One budget for the whole message: the message path waits for this call, and
	// four slow pictures must not add up to four waits.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	refs := make([]string, 0, len(urls))
	for index, url := range urls {
		ref, err := c.fetch(ctx, messageID, index, url)
		if err != nil {
			// Debug, not warn: an attachment that has already gone is a fact of
			// life for a presigned URL, and one line per picture would be noise.
			c.logDebug("could not keep a picture", "message", messageID,
				"position", index, "error", err)
			continue
		}
		refs = append(refs, ref)
	}
	c.sweepIfDue(ctx)
	return refs
}

// fetch downloads one picture, shapes it and writes it down.
func (c *Cache) fetch(ctx context.Context, messageID string, index int, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building the download request: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the picture answered %s", response.Status)
	}

	limit := c.readLimit()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("reading the picture: %w", err)
	}
	if int64(len(body)) > limit {
		return "", fmt.Errorf("the picture is larger than the %d bytes read from it", limit)
	}

	stored, extension, err := c.shape(body)
	if err != nil {
		return "", err
	}
	if c.maxBytes > 0 && int64(len(stored)) > c.maxBytes {
		return "", fmt.Errorf("the picture is %d bytes, over the %d byte limit",
			len(stored), c.maxBytes)
	}

	ref := refName(messageID, index, extension)
	if err := c.write(ref, stored); err != nil {
		return "", err
	}
	return ref, nil
}

// readLimit is how much of one picture is read at all.
//
// Never the storing limit itself: a picture over that limit is one to be shrunk
// first, and a read that stopped at the limit would refuse it before it could be.
func (c *Cache) readLimit() int64 {
	if c.maxBytes > hardCeiling {
		return c.maxBytes
	}
	return hardCeiling
}

// shape is the picture as it is to be stored: the original where that will do, a
// shrunk and re-encoded copy where it will not.
//
// It returns the extension the stored bytes want, which is not the one they arrived
// with: a shrunk picture comes back as a JPEG whatever it was.
func (c *Cache) shape(body []byte) ([]byte, string, error) {
	mime := sniff(body)
	if mime == "" {
		// The message said this was a picture and what arrived is not one: an error
		// page, a login wall, a redirect the client followed. Keeping it would put
		// something in front of the model that is not what the member posted.
		return nil, "", errors.New("what arrived is not a picture")
	}
	extension := extensionFor(mime)
	if c.compressAbove <= 0 || int64(len(body)) <= c.compressAbove {
		return body, extension, nil
	}

	shrunk, err := shrink(body, mime, c.maxEdge)
	if err != nil {
		// A picture that cannot be shrunk is kept as it is when it fits: an
		// animated GIF is the case that matters, and re-encoding it would throw
		// away the part of it that moves -- which is what a GIF advertisement is
		// for.
		c.logDebug("could not shrink a picture, so it is kept as it is", "error", err)
		return body, extension, nil
	}
	if len(shrunk) >= len(body) {
		// Shrinking that grew the file is not shrinking. It happens on a small
		// picture already saved at a lower quality than this one writes.
		return body, extension, nil
	}
	return shrunk, ".jpg", nil
}

// shrink re-encodes a picture smaller: longest side capped, JPEG out.
//
// The pixels are averaged over the boxes they are being collapsed into, which is
// the one resampling this file needs: the picture is being made smaller for a model
// that will scale it anyway, and a dependency on a resampling library would be a
// dependency for nothing else.
func shrink(body []byte, mime string, maxEdge int) ([]byte, error) {
	if mime == mimeGIF {
		// Decoded in full first, because only the frame count says whether the
		// picture moves, and image.Decode answers with the first frame alone.
		frames, err := gif.DecodeAll(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("reading the GIF: %w", err)
		}
		if len(frames.Image) > 1 {
			return nil, errors.New("the GIF is animated")
		}
	}
	source, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decoding the picture: %w", err)
	}
	var out bytes.Buffer
	// Alpha is dropped rather than hidden: JPEG has none, and a picture with a
	// transparent background is a picture whose subject is opaque.
	if err := jpeg.Encode(&out, scale(source, maxEdge), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("encoding the shrunk picture: %w", err)
	}
	return out.Bytes(), nil
}

// scale returns the picture with its longest side capped at maxEdge, unchanged
// when it already fits.
func scale(source image.Image, maxEdge int) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maxEdge && height <= maxEdge {
		return source
	}
	factor := float64(maxEdge) / float64(width)
	if height > width {
		factor = float64(maxEdge) / float64(height)
	}
	targetWidth := int(float64(width)*factor + 0.5)
	targetHeight := int(float64(height)*factor + 0.5)
	if targetWidth < 1 {
		targetWidth = 1
	}
	if targetHeight < 1 {
		targetHeight = 1
	}

	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		top := bounds.Min.Y + y*height/targetHeight
		bottom := bounds.Min.Y + (y+1)*height/targetHeight
		if bottom <= top {
			bottom = top + 1
		}
		for x := 0; x < targetWidth; x++ {
			left := bounds.Min.X + x*width/targetWidth
			right := bounds.Min.X + (x+1)*width/targetWidth
			if right <= left {
				right = left + 1
			}
			var red, green, blue, alpha, count uint32
			for sourceY := top; sourceY < bottom && sourceY < bounds.Max.Y; sourceY++ {
				for sourceX := left; sourceX < right && sourceX < bounds.Max.X; sourceX++ {
					r, g, b, a := source.At(sourceX, sourceY).RGBA()
					red += r >> 8
					green += g >> 8
					blue += b >> 8
					alpha += a >> 8
					count++
				}
			}
			if count == 0 {
				continue
			}
			offset := target.PixOffset(x, y)
			target.Pix[offset] = uint8(red / count)
			target.Pix[offset+1] = uint8(green / count)
			target.Pix[offset+2] = uint8(blue / count)
			target.Pix[offset+3] = uint8(alpha / count)
		}
	}
	return target
}

// Read returns one stored picture.
//
// The reference is a name this cache wrote and never a path: a cached message comes
// back from Redis, which is shared with whatever else uses the server, so the name
// in it is not trusted to stay inside the directory.
func (c *Cache) Read(ref string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("imagecache: no cache is configured")
	}
	if !safeRef(ref) {
		return nil, fmt.Errorf("imagecache: %q is not a name this cache wrote", ref)
	}
	return os.ReadFile(filepath.Join(c.dir, ref))
}

// MIME is the type a stored picture is served as.
func (c *Cache) MIME(ref string) string {
	if c == nil {
		return ""
	}
	return mimeFor(extension(ref))
}

// Sweep removes the pictures that have outlived the messages they came with.
//
// What is looked at is the file's own age rather than anything written down: a
// picture is written once and never touched again, so its modification time is
// when its message was cached, which is the same clock the message cache prunes by.
func (c *Cache) Sweep(ctx context.Context) {
	if c == nil || c.retention <= 0 {
		return
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		c.logDebug("could not read the image cache directory", "error", err)
		return
	}
	cutoff := time.Now().Add(-c.retention)
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(c.dir, entry.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		c.logDebug("pictures that outlived their messages were removed", "count", removed)
	}
}

// sweepIfDue runs the sweep at most once every so often.
func (c *Cache) sweepIfDue(ctx context.Context) {
	c.mu.Lock()
	due := time.Since(c.lastSweep) >= sweepInterval
	if due {
		c.lastSweep = time.Now()
	}
	c.mu.Unlock()
	if due {
		c.Sweep(ctx)
	}
}

// write puts the bytes in the directory under the reference, all or nothing.
//
// Written beside its name and moved onto it, so that a picture is either there
// whole or not there: a reader that found half a JPEG would fail to decode it and
// lose the picture anyway.
func (c *Cache) write(ref string, body []byte) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("creating the image cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(c.dir, ".writing-*")
	if err != nil {
		return fmt.Errorf("writing a picture: %w", err)
	}
	name := temporary.Name()
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		os.Remove(name)
		return fmt.Errorf("writing a picture: %w", err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing a picture: %w", err)
	}
	if err := os.Rename(name, filepath.Join(c.dir, ref)); err != nil {
		os.Remove(name)
		return fmt.Errorf("storing a picture: %w", err)
	}
	return nil
}

// refName is the name a picture is stored under.
//
// Hashed rather than taken from the message id: the id comes from the platform and
// is not a filename, and hashing makes the same picture of the same message land on
// the same name however many times the event is delivered -- which the platform
// does do, sending a mention twice, once as an ordinary message.
func refName(messageID string, index int, extension string) string {
	sum := sha256.Sum256([]byte(messageID + "\x00" + strconv.Itoa(index)))
	return hex.EncodeToString(sum[:refBytes]) + extension
}

// safeRef reports whether a name is one this cache could have written.
func safeRef(ref string) bool {
	if ref == "" || !utf8.ValidString(ref) {
		return false
	}
	if ref != filepath.Base(ref) || strings.ContainsRune(ref, os.PathSeparator) {
		return false
	}
	if strings.Contains(ref, "..") || strings.HasPrefix(ref, ".") {
		return false
	}
	return true
}

// The picture types the platform delivers, and the extensions they are stored
// under.
const (
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
	mimeGIF  = "image/gif"
)

// sniff names the picture in the bytes, or says nothing when it is not one.
func sniff(body []byte) string {
	switch http.DetectContentType(body) {
	case mimeJPEG:
		return mimeJPEG
	case mimePNG:
		return mimePNG
	case mimeGIF:
		return mimeGIF
	default:
		return ""
	}
}

// extensionFor is the extension a picture type is stored under.
func extensionFor(mime string) string {
	switch mime {
	case mimePNG:
		return ".png"
	case mimeGIF:
		return ".gif"
	default:
		return ".jpg"
	}
}

// extension is the extension a stored name ends in, lower case.
func extension(ref string) string {
	return strings.ToLower(filepath.Ext(ref))
}

// mimeFor is the type a stored extension is served as.
func mimeFor(extension string) string {
	switch extension {
	case ".png":
		return mimePNG
	case ".gif":
		return mimeGIF
	default:
		return mimeJPEG
	}
}

// logDebug says something about a picture that was not kept, when anybody is
// listening.
func (c *Cache) logDebug(message string, args ...any) {
	if c.log != nil {
		c.log.Debug(message, args...)
	}
}
