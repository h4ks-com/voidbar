package mirror

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestDecodeCompressedPassesTextThrough(t *testing.T) {
	src := []byte("(this.webpackChunkdiscord_app=[]).push([[1],{}]);")
	out, ok := DecodeCompressed("/assets/x.js", src)
	if !ok || !bytes.Equal(out, src) {
		t.Fatalf("textual input must pass through: ok=%v", ok)
	}
}

func TestDecodeCompressedRepairsBrotli(t *testing.T) {
	src := []byte("(this.webpackChunkdiscord_app=[]).push([[62884],{\"a\":1}]);")
	var compressed bytes.Buffer
	w := brotli.NewWriter(&compressed)
	if _, err := w.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := DecodeCompressed("/assets/62884.abc.js", compressed.Bytes())
	if !ok || !bytes.Equal(out, src) {
		t.Fatalf("brotli body must decode: ok=%v match=%v", ok, bytes.Equal(out, src))
	}
}

func TestDecodeCompressedRepairsGzip(t *testing.T) {
	src := []byte("body{color:red}")
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := DecodeCompressed("/assets/x.css", compressed.Bytes())
	if !ok || !bytes.Equal(out, src) {
		t.Fatalf("gzip body must decode: ok=%v", ok)
	}
}

func TestDecodeCompressedLeavesBinaryAssets(t *testing.T) {
	src := []byte{0x00, 0x01, 0x89, 0xff, 0x50, 0x4e, 0x47}
	out, ok := DecodeCompressed("/assets/font.woff2", src)
	if !ok || !bytes.Equal(out, src) {
		t.Fatalf("binary assets must never be rewritten: ok=%v", ok)
	}
}

func TestDecodeCompressedRejectsGarbage(t *testing.T) {
	// Random-ish binary that is neither gzip nor brotli: must report not-ok
	// so the caller can re-fetch instead of saving garbage.
	src := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 64)
	if _, ok := DecodeCompressed("/assets/x.js", src); ok {
		t.Fatal("undecodable body must not be accepted as .js")
	}
}
