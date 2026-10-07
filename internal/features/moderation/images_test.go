package moderation

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeImages is one images section as the feature reads it.
func decodeImages(t *testing.T, section string) Images {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("dir: \"\"\n"+section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	var images Images
	if err := document.Content[0].Decode(&images); err != nil {
		t.Fatalf("reading the images section: %v", err)
	}
	if err := images.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	return images
}

// TestAnEmptyImagesSectionIsAWorkingOne covers the defaults: a deployment that writes
// no images block at all still keeps pictures, and an operator reading the file can
// see what those numbers are.
func TestAnEmptyImagesSectionIsAWorkingOne(t *testing.T) {
	images := decodeImages(t, "")

	if strings.TrimSpace(images.Dir) == "" {
		t.Error("no directory: the pictures would have nowhere to go")
	}
	if got := images.DownloadTimeoutSeconds; got != DefaultImageDownloadSeconds {
		t.Errorf("download_timeout_seconds = %d, want %d", got, DefaultImageDownloadSeconds)
	}
	if images.MaxPerMessage == nil || *images.MaxPerMessage != DefaultImagesPerMessage {
		t.Errorf("max_per_message = %v, want %d", images.MaxPerMessage,
			DefaultImagesPerMessage)
	}
	if images.CompressAboveBytes == nil ||
		*images.CompressAboveBytes != DefaultImageCompressAboveBytes {
		t.Errorf("compress_above_bytes = %v, want %d", images.CompressAboveBytes,
			DefaultImageCompressAboveBytes)
	}
	if images.MaxBytes == nil || *images.MaxBytes != DefaultImageMaxBytes {
		t.Errorf("max_bytes = %v, want %d", images.MaxBytes, DefaultImageMaxBytes)
	}
	if got := images.MaxEdge; got != DefaultImageMaxEdge {
		t.Errorf("max_edge = %d, want %d", got, DefaultImageMaxEdge)
	}
}

// TestZeroTurnsTheImageLimitsOff covers what a written zero means for the three
// limits, which is the reason they are pointers: leaving the field out takes the
// default, and writing a zero turns the limit off.
func TestZeroTurnsTheImageLimitsOff(t *testing.T) {
	images := decodeImages(t, "compress_above_bytes: 0\nmax_bytes: 0\nmax_per_message: 0\n")

	if images.CompressAboveBytes == nil || *images.CompressAboveBytes != 0 {
		t.Errorf("compress_above_bytes = %v, want zero, which is compression off",
			images.CompressAboveBytes)
	}
	if images.MaxBytes == nil || *images.MaxBytes != 0 {
		t.Errorf("max_bytes = %v, want zero, which is no size limit", images.MaxBytes)
	}
	if images.MaxPerMessage == nil || *images.MaxPerMessage != 0 {
		t.Errorf("max_per_message = %v, want zero, which keeps every picture",
			images.MaxPerMessage)
	}
}

// TestTheImageLimitsAsWrittenAreKept covers the other direction: what the file says
// is what the cache is built with.
func TestTheImageLimitsAsWrittenAreKept(t *testing.T) {
	images := decodeImages(t, "compress_above_bytes: 1024\nmax_bytes: 4096\n"+
		"max_per_message: 2\nmax_edge: 800\ndownload_timeout_seconds: 3\n")

	if *images.CompressAboveBytes != 1024 || *images.MaxBytes != 4096 ||
		*images.MaxPerMessage != 2 || images.MaxEdge != 800 ||
		images.DownloadTimeoutSeconds != 3 {
		t.Errorf("the section was defaulted over: %+v", images)
	}
}

// TestACompressionThresholdAboveTheLimitIsRefused covers the pair that cannot both be
// meant.
//
// A picture over the compression threshold is one to be shrunk before it is sent, and
// a limit below that threshold would drop it before it could be -- so the two numbers
// together would mean every picture over the threshold is dropped while the operator
// believed they were being shrunk.
func TestACompressionThresholdAboveTheLimitIsRefused(t *testing.T) {
	var document yaml.Node
	section := "compress_above_bytes: 4096\nmax_bytes: 1024\n"
	if err := yaml.Unmarshal([]byte(section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	var images Images
	if err := document.Content[0].Decode(&images); err != nil {
		t.Fatalf("reading the images section: %v", err)
	}
	err := images.applyDefaults()
	if err == nil {
		t.Fatal("a compression threshold above the size limit was accepted")
	}
	if !strings.Contains(err.Error(), "compress_above_bytes") {
		t.Errorf("the refusal does not name the field: %v", err)
	}

	// And with compression off there is no such pair to be wrong: the limit stands
	// on its own.
	section = "compress_above_bytes: 0\nmax_bytes: 1024\n"
	if err := yaml.Unmarshal([]byte(section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	images = Images{}
	if err := document.Content[0].Decode(&images); err != nil {
		t.Fatalf("reading the images section: %v", err)
	}
	if err := images.applyDefaults(); err != nil {
		t.Errorf("a size limit with compression off was refused: %v", err)
	}
}
