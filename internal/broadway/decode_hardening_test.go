package broadway

import (
	"bytes"
	"compress/flate"
	"errors"
	"testing"
)

func TestDecodeBufferRejectsOversizedDimensions(t *testing.T) {
	for _, dims := range [][2]int{{0, 1}, {1, 0}, {65535, 65535}, {maxSurfaceDim + 1, 1}} {
		if _, err := decodeBuffer(nil, 0, 0, dims[0], dims[1], nil); err == nil {
			t.Fatalf("dims %v: expected error", dims)
		}
	}
}

func TestDecodeBufferMalformedInputNeverPanics(t *testing.T) {
	// Each payload should return an error or a correctly sized buffer, never panic.
	payloads := [][]byte{
		{0x00, 0x00, 0x00, 0x00, 0x10, 0xff, 0xff, 0xff},             // delta0 far past the output
		{0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00},             // zero-length header then short
		{0x00, 0x00, 0x30, 0x00, 0xff, 0xff, 0xff, 0x01, 1, 2, 3, 4}, // huge color run
		{0x00, 0x00, 0x40, 0x00, 0xff, 0xff, 0xff, 0x01, 1, 2, 3, 4}, // huge delta run
		{0x00, 0x00, 0x20, 0x00, 0xff, 0xff, 0xff, 0xff},             // block ref, truncated
		{0x00, 0x00, 0x50, 0x00},                                     // unknown command
		{1, 2, 3, 0},                                                 // truncated transparent pixel
	}
	for i, p := range payloads {
		out, err := decodeBuffer(nil, 0, 0, 2, 2, p)
		if err == nil && len(out) != 16 {
			t.Fatalf("payload %d: got %d bytes, want 16", i, len(out))
		}
	}
}

func TestDecodeBufferOverflowIsCorrupt(t *testing.T) {
	// A colour run of 0xFFFFFF pixels into a 1x1 buffer must fail with ErrCorrupt.
	data := []byte{0x00, 0x00, 0x3f, 0x00, 0xff, 0xff, 0xff, 0x01, 1, 2, 3, 4}
	_, err := decodeBuffer(nil, 0, 0, 1, 1, data)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestInflateRawCapsOutput(t *testing.T) {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	chunk := make([]byte, 1<<20)
	for i := 0; i < (maxInflatedBytes>>20)+2; i++ {
		_, _ = w.Write(chunk) // zeros compress to almost nothing
	}
	_ = w.Close()
	if _, err := inflateRaw(buf.Bytes()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt for inflated payload over cap", err)
	}
}

func TestApplyServerMessageRejectsHugeSurface(t *testing.T) {
	surfaces := map[uint16]*Surface{}
	// PUT_BUFFER with w=h=65535 must be refused before any large allocation.
	msg := []byte{'b', 1, 0, 0, 0, 1, 0, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}
	if err := ApplyServerMessage(msg, surfaces); err == nil {
		t.Fatal("expected error for oversized surface")
	}
}

func TestDecodeBufferSkipRunAsLastCommand(t *testing.T) {
	// One opaque pixel, then a delta0 (skip) run of one pixel as the final word.
	// The skip word carries its own length, so nothing may follow it.
	data := []byte{
		50, 100, 200, 255, // opaque pixel (b, g, r, a)
		1, 0, 0x10, 0, // delta0: run length 1
	}
	out, err := decodeBuffer(nil, 0, 0, 2, 1, data)
	if err != nil {
		t.Fatalf("trailing skip run rejected: %v", err)
	}
	if len(out) != 8 || out[0] != 200 || out[3] != 255 || out[7] != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestDecodeBufferSkipRunPastEndIsCorrupt(t *testing.T) {
	data := []byte{5, 0, 0x10, 0} // skip 5 pixels in a 1x1 buffer
	if _, err := decodeBuffer(nil, 0, 0, 1, 1, data); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestPutBufferDiffsAgainstOldBufferSizeAfterResize(t *testing.T) {
	// A 'm' op already grew the surface to 5x5, but its buffer is still the old 2x1 one.
	// A buffer that only skips both pixels must keep the old pixels, which requires
	// decoding against the buffer's own 2x1 size and not the surface's 5x5.
	old := []byte{10, 20, 30, 255, 40, 50, 60, 255}
	surfaces := map[uint16]*Surface{1: {ID: 1, Width: 5, Height: 5, Visible: true, Pixels: old, PixelsW: 2, PixelsH: 1}}

	var payload bytes.Buffer
	w, _ := flate.NewWriter(&payload, flate.DefaultCompression)
	_, _ = w.Write([]byte{2, 0, 0x10, 0}) // delta0: skip 2 pixels
	_ = w.Close()
	msg := []byte{'b', 1, 0, 0, 0, 1, 0, 2, 0, 1, 0, byte(payload.Len()), 0, 0, 0}
	msg = append(msg, payload.Bytes()...)

	if err := ApplyServerMessage(msg, surfaces); err != nil {
		t.Fatal(err)
	}
	s := surfaces[1]
	if s.PixelsW != 2 || s.PixelsH != 1 || s.Width != 2 || s.Height != 1 {
		t.Fatalf("dimensions not updated: %+v", s)
	}
	if !bytes.Equal(s.Pixels, old) {
		t.Fatalf("old pixels lost after resize: %v", s.Pixels)
	}
}
