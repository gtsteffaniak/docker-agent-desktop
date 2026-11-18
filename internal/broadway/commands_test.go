package broadway

import (
	"encoding/binary"
	"testing"
)

func TestApplyServerMessageCreateAndShow(t *testing.T) {
	surfaces := map[uint16]*Surface{}
	var msg []byte
	put := func(b ...byte) { msg = append(msg, b...) }
	putU16 := func(v uint16) {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		put(b[:]...)
	}
	putU32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		put(b[:]...)
	}

	put('s')
	putU32(1)
	putU16(2)
	putU16(0xFFF0) // x signed as u16 bits
	putU16(0xFFF0) // y
	putU16(32)
	putU16(24)
	put(0) // isTemp false

	put('S')
	putU32(2)
	putU16(2)

	if err := ApplyServerMessage(msg, surfaces); err != nil {
		t.Fatal(err)
	}
	s := surfaces[2]
	if s == nil || !s.Visible || s.Width != 32 {
		t.Fatalf("surface=%+v", s)
	}
}
