//go:build linux

// Package qtfb is a pure-Go client for the AppLoad/qtfb shared-memory
// framebuffer protocol used to run windowed apps inside xochitl on the
// reMarkable Paper Pro.
//
// Protocol reference: rmpp-appload/backends/qtfb-clients/cpp/common.h
package qtfb

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	SocketPath         = "/tmp/qtfb.sock"
	DefaultFramebuffer = 245209899

	RMPPWidth  = 1620
	RMPPHeight = 2160

	msgInitialize     = 0
	msgUpdate         = 1
	msgTerminate      = 3
	msgUserInput      = 4
	msgSetRefreshMode = 5
	msgFullRefresh    = 6

	FmtRMPPRGB888   = 1
	FmtRMPPRGBA8888 = 2
	FmtRMPPRGB565   = 3

	updateAll     = 0
	updatePartial = 1

	RefreshUltraFast = 0
	RefreshFast      = 1
	RefreshAnimate   = 2
	RefreshContent   = 3
	RefreshUI        = 4

	InputTouchPress   = 0x10
	InputTouchRelease = 0x11
	InputTouchUpdate  = 0x12
	InputPenPress     = 0x20
	InputPenRelease   = 0x21
	InputPenUpdate    = 0x22
)

// sizeof(struct ClientMessage) / sizeof(struct ServerMessage) on aarch64.
const (
	clientMsgSize = 24
	serverMsgSize = 32
)

var le = binary.LittleEndian

type InputEvent struct {
	Type  int32
	DevID int32
	X, Y  int32
	D     int32
}

type Client struct {
	fd     int
	shmFD  int
	Shm    []byte
	Width  int
	Height int

	// Input receives touch/pen events; Terminated is closed when the
	// server asks the app to exit.
	Input      chan InputEvent
	Terminated chan struct{}
}

// KeyFromEnv returns the framebuffer key AppLoad assigned via QTFB_KEY,
// or the default shared key when launched outside AppLoad.
func KeyFromEnv() uint32 {
	if v := os.Getenv("QTFB_KEY"); v != "" {
		if k, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(k)
		}
	}
	return DefaultFramebuffer
}

func Connect(key uint32, format uint8) (*Client, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: SocketPath}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("connect %s: %w", SocketPath, err)
	}

	// struct ClientMessage { uint8 type; union { FBKey key; uint8 fbType @+4 } @4 }
	msg := make([]byte, clientMsgSize)
	msg[0] = msgInitialize
	le.PutUint32(msg[4:], key)
	msg[8] = format
	if err := unix.Send(fd, msg, 0); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("send init: %w", err)
	}

	buf := make([]byte, serverMsgSize)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil || n < serverMsgSize {
		unix.Close(fd)
		return nil, fmt.Errorf("recv init response (n=%d): %w", n, err)
	}
	shmKey := int32(le.Uint32(buf[8:]))
	shmSize := le.Uint64(buf[16:])

	shmPath := fmt.Sprintf("/dev/shm/qtfb_%d", shmKey)
	shmFD, err := unix.Open(shmPath, unix.O_RDWR, 0)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("open %s: %w", shmPath, err)
	}
	mem, err := unix.Mmap(shmFD, 0, int(shmSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(shmFD)
		unix.Close(fd)
		return nil, fmt.Errorf("mmap: %w", err)
	}

	c := &Client{
		fd: fd, shmFD: shmFD, Shm: mem,
		Width: RMPPWidth, Height: RMPPHeight,
		Input:      make(chan InputEvent, 64),
		Terminated: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	buf := make([]byte, serverMsgSize)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil || n == 0 {
			close(c.Terminated)
			return
		}
		switch buf[0] {
		case msgTerminate:
			close(c.Terminated)
			return
		case msgUserInput:
			ev := InputEvent{
				Type:  int32(le.Uint32(buf[8:])),
				DevID: int32(le.Uint32(buf[12:])),
				X:     int32(le.Uint32(buf[16:])),
				Y:     int32(le.Uint32(buf[20:])),
				D:     int32(le.Uint32(buf[24:])),
			}
			select {
			case c.Input <- ev:
			default: // drop if the app is behind
			}
		}
	}
}

func (c *Client) send(msg []byte) error {
	return unix.Send(c.fd, msg, 0)
}

// FullUpdate flushes the whole shared framebuffer to the display.
func (c *Client) FullUpdate() error {
	msg := make([]byte, clientMsgSize)
	msg[0] = msgUpdate
	le.PutUint32(msg[4:], updateAll)
	return c.send(msg)
}

// PartialUpdate flushes only the given rectangle.
func (c *Client) PartialUpdate(x, y, w, h int) error {
	msg := make([]byte, clientMsgSize)
	msg[0] = msgUpdate
	le.PutUint32(msg[4:], updatePartial)
	le.PutUint32(msg[8:], uint32(x))
	le.PutUint32(msg[12:], uint32(y))
	le.PutUint32(msg[16:], uint32(w))
	le.PutUint32(msg[20:], uint32(h))
	return c.send(msg)
}

// SetRefreshMode selects the e-ink waveform (Refresh* constants).
func (c *Client) SetRefreshMode(mode int) error {
	msg := make([]byte, clientMsgSize)
	msg[0] = msgSetRefreshMode
	le.PutUint32(msg[4:], uint32(mode))
	return c.send(msg)
}

// RequestFullRefresh asks the display for a deep (flashing) refresh,
// clearing accumulated e-ink ghosting.
func (c *Client) RequestFullRefresh() error {
	msg := make([]byte, clientMsgSize)
	msg[0] = msgFullRefresh
	return c.send(msg)
}

func (c *Client) Close() {
	unix.Munmap(c.Shm)
	unix.Close(c.shmFD)
	unix.Close(c.fd)
}
