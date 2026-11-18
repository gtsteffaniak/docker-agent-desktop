package broadway

import (
	"bytes"
	"compress/flate"
	"testing"
)

func TestDecodeBufferOpaquePixel(t *testing.T) {
	data := []byte{50, 100, 200, 255}
	out, err := decodeBuffer(nil, 0, 0, 1, 1, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0] != 200 || out[1] != 100 || out[2] != 50 || out[3] != 255 {
		t.Fatalf("rgba=%v", out)
	}
}

func TestDecodeBufferTransparentPixel(t *testing.T) {
	data := []byte{0x00, 0x00, 0x00, 0x00}
	out, err := decodeBuffer(nil, 0, 0, 1, 1, data)
	if err != nil {
		t.Fatal(err)
	}
	if out[3] != 0 {
		t.Fatalf("alpha=%d", out[3])
	}
}

func TestInflateRawAndApplyPutBuffer(t *testing.T) {
	plain := []byte{200, 100, 50, 255}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	_, _ = w.Write(plain)
	_ = w.Close()

	surfaces := map[uint16]*Surface{}
	// op 'b': serial u32, id u16, w u16, h u16, compressed
	msg := []byte{'b', 1, 0, 0, 0, 1, 0, 1, 0, 1, 0, byte(len(buf.Bytes())), 0, 0, 0}
	msg = append(msg, buf.Bytes()...)
	if err := ApplyServerMessage(msg, surfaces); err != nil {
		t.Fatal(err)
	}
	s := surfaces[1]
	if s == nil || len(s.Pixels) != 4 {
		t.Fatalf("surface=%v", s)
	}
}

func TestInputEncoderLayout(t *testing.T) {
	e := NewInputEncoder()
	m := e.SetScreenSize(1280, 800)
	if len(m) != 5*4 {
		t.Fatalf("len=%d", len(m))
	}
	if m[0] != 0 || m[3] != 'd' {
		t.Fatalf("opcode bytes=%v", m[:4])
	}
}
