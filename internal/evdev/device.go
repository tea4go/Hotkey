package evdev

import (
	"io"
	"os"
	"sync"
)

type Device struct {
	Path   string
	Name   string
	fd     *os.File
	events chan Event
	closed chan struct{}
	once   sync.Once
}

func (d *Device) Events() <-chan Event {
	return d.events
}

func (d *Device) Close() error {
	d.once.Do(func() {
		close(d.closed)
		d.fd.Close()
	})
	return nil
}

func (d *Device) readLoop() {
	defer close(d.events)
	buf := make([]byte, InputEvSize)
	for {
		_, err := io.ReadFull(d.fd, buf)
		if err != nil {
			return
		}
		typ, code, value := parseInputEvent(buf)
		if typ != EvKey {
			continue
		}
		select {
		case d.events <- Event{DevicePath: d.Path, Type: typ, Code: code, Value: value}:
		case <-d.closed:
			return
		}
	}
}

func openDevice(path, name string, fd *os.File) *Device {
	d := &Device{
		Path:   path,
		Name:   name,
		fd:     fd,
		events: make(chan Event),
		closed: make(chan struct{}),
	}
	go d.readLoop()
	return d
}
