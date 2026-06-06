package evdev

import "encoding/binary"

const (
	EvSyn       uint16 = 0x00
	EvKey       uint16 = 0x01
	InputEvSize        = 24
)

type Event struct {
	DevicePath string
	Type       uint16
	Code       uint16
	Value      int32
}

func parseInputEvent(buf []byte) (typ, code uint16, value int32) {
	typ = binary.LittleEndian.Uint16(buf[16:18])
	code = binary.LittleEndian.Uint16(buf[18:20])
	value = int32(binary.LittleEndian.Uint32(buf[20:24]))
	return
}
