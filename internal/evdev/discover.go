package evdev

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	evBits   = 0x20
	keyMax   = 0x2ff
	bitmapSz = (keyMax + 7) / 8

	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocRead = 2

	nameLen = 256
)

var letterKeycodes = []int{
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
	30, 31, 32, 33, 34, 35, 36, 37, 38,
	44, 45, 46, 47, 48, 49, 50,
}

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) |
		(nr << iocNRShift) | (size << iocSizeShift)
}

func eviocgname(l int) uintptr {
	return ioc(iocRead, 'E', 0x06, uintptr(l))
}

func eviocgbit(ev, l int) uintptr {
	return ioc(iocRead, 'E', uintptr(evBits+ev), uintptr(l))
}

func isKeyboard(bitmap []byte) bool {
	count := 0
	for _, code := range letterKeycodes {
		byteIdx := code / 8
		bitIdx := uint(code % 8)
		if byteIdx < len(bitmap) && (bitmap[byteIdx]&(1<<bitIdx)) != 0 {
			count++
		}
	}
	return count >= 20
}

func getDeviceName(fd int) (string, error) {
	nameBuf := make([]byte, nameLen)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		eviocgname(nameLen),
		uintptr(unsafe.Pointer(&nameBuf[0])),
	)
	if errno != 0 {
		return "", fmt.Errorf("ioctl EVIOCGNAME: %v", errno)
	}
	for i, b := range nameBuf {
		if b == 0 {
			return string(nameBuf[:i]), nil
		}
	}
	return string(nameBuf), nil
}

func getKeyBitmap(fd int) ([]byte, error) {
	bitmap := make([]byte, bitmapSz)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		eviocgbit(int(EvKey), bitmapSz),
		uintptr(unsafe.Pointer(&bitmap[0])),
	)
	if errno != 0 {
		return nil, fmt.Errorf("ioctl EVIOCGBIT: %v", errno)
	}
	return bitmap, nil
}

func tryOpenDevice(path string) (*Device, error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	intFd := int(fd.Fd())

	name, err := getDeviceName(intFd)
	if err != nil {
		fd.Close()
		return nil, err
	}

	bitmap, err := getKeyBitmap(intFd)
	if err != nil {
		fd.Close()
		return nil, err
	}

	if !isKeyboard(bitmap) {
		fd.Close()
		return nil, fmt.Errorf("not a keyboard")
	}

	return openDevice(path, name, fd), nil
}

func Discover() ([]*Device, error) {
	var devices []*Device
	permissionDenied := 0

	for i := 0; i < 32; i++ {
		path := fmt.Sprintf("/dev/input/event%d", i)
		dev, err := tryOpenDevice(path)
		if err != nil {
			if os.IsPermission(err) {
				permissionDenied++
			}
			continue
		}
		devices = append(devices, dev)
	}

	if len(devices) == 0 && permissionDenied > 0 {
		return nil, fmt.Errorf("无法打开任何输入设备（需要 root 权限或加入 input 组）")
	}

	return devices, nil
}
