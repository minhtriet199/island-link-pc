package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The Remote screen: a virtual mouse + keyboard made with /dev/uinput (no daemon, no extra tool).
// It exists only while the screen is open. Text that isn't plain ASCII goes through wtype when
// that is installed, because a virtual keyboard can only press keys of the current layout.

const (
	evSyn, evKey, evRel       = 0, 1, 2
	relX, relY, relWheel      = 0, 1, 8
	btnLeft, btnRight, btnMid = 0x110, 0x111, 0x112
	keyLeftShift              = 42
	keyLeftMeta               = 125

	uiSetEvBit  = 0x40045564
	uiSetKeyBit = 0x40045565
	uiSetRelBit = 0x40045566
	uiDevSetup  = 0x405c5503
	uiDevCreate = 0x5501
	uiDevDest   = 0x5502
)

// namedKeys are the buttons on the Remote screen.
var namedKeys = map[string]uint16{
	"esc": 1, "backspace": 14, "tab": 15, "enter": 28, "up": 103, "left": 105, "right": 106, "down": 108,
	"home": 102, "end": 107, "pageup": 104, "pagedown": 109, "delete": 111, "super": keyLeftMeta,
}

// asciiKey maps a character to its key on a US layout; ok is false for anything else.
func asciiKey(r rune) (code uint16, shift, ok bool) {
	const rows = "1234567890-=\x00\x00qwertyuiop[]\x00\x00asdfghjkl;'`\x00\\zxcvbnm,./"
	const shifted = "!@#$%^&*()_+\x00\x00QWERTYUIOP{}\x00\x00ASDFGHJKL:\"~\x00|ZXCVBNM<>?"
	switch r {
	case ' ':
		return 57, false, true
	case '\n':
		return 28, false, true
	case '\t':
		return 15, false, true
	case 0:
		return 0, false, false
	}
	if i := strings.IndexRune(rows, r); i >= 0 {
		return uint16(i + 2), false, true
	}
	if i := strings.IndexRune(shifted, r); i >= 0 {
		return uint16(i + 2), true, true
	}
	return 0, false, false
}

type inputDevice struct{ f *os.File }

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func openInput() (*inputDevice, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	fd := f.Fd()
	bad := func(err error) (*inputDevice, error) {
		f.Close()
		return nil, err
	}
	for _, ev := range []uintptr{evKey, evRel} {
		if err := ioctl(fd, uiSetEvBit, ev); err != nil {
			return bad(err)
		}
	}
	for _, r := range []uintptr{relX, relY, relWheel} {
		if err := ioctl(fd, uiSetRelBit, r); err != nil {
			return bad(err)
		}
	}
	keys := []uintptr{btnLeft, btnRight, btnMid}
	for k := uintptr(1); k < 58; k++ {
		keys = append(keys, k)
	}
	for _, k := range namedKeys {
		keys = append(keys, uintptr(k))
	}
	for _, k := range keys {
		if err := ioctl(fd, uiSetKeyBit, k); err != nil {
			return bad(err)
		}
	}
	var setup struct {
		Bustype, Vendor, Product, Version uint16
		Name                              [80]byte
		FFMax                             uint32
	}
	setup.Bustype, setup.Vendor, setup.Product, setup.Version = 0x06, 0x1209, 0x1314, 1 // BUS_VIRTUAL
	copy(setup.Name[:], "Island Link Remote")
	if err := ioctl(fd, uiDevSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
		return bad(err)
	}
	if err := ioctl(fd, uiDevCreate, 0); err != nil {
		return bad(err)
	}
	return &inputDevice{f}, nil
}

func (d *inputDevice) close() {
	ioctl(d.f.Fd(), uiDevDest, 0)
	d.f.Close()
}

// emit writes one batch of events and the report that ends it.
func (d *inputDevice) emit(events ...[3]int32) error {
	var b bytes.Buffer
	put := func(typ, code uint16, value int32) {
		binary.Write(&b, binary.LittleEndian, struct {
			Sec, Usec int64
			Type,
			Code uint16
			Value int32
		}{0, 0, typ, code, value})
	}
	for _, e := range events {
		put(uint16(e[0]), uint16(e[1]), e[2])
	}
	put(evSyn, 0, 0)
	_, err := d.f.Write(b.Bytes())
	return err
}

func (d *inputDevice) press(code uint16, on bool) error {
	v := int32(0)
	if on {
		v = 1
	}
	return d.emit([3]int32{evKey, int32(code), v})
}

func (d *inputDevice) tap(code uint16, shift bool) error {
	if shift {
		d.press(keyLeftShift, true)
		defer d.press(keyLeftShift, false)
	}
	d.press(code, true)
	return d.press(code, false)
}

// typeText types ASCII on the virtual keyboard; the whole text goes to wtype if any of it isn't.
func (d *inputDevice) typeText(text string) error {
	for _, r := range text {
		if _, _, ok := asciiKey(r); !ok {
			if _, err := exec.LookPath("wtype"); err != nil {
				return fmt.Errorf("accented letters need wtype on the PC")
			}
			return exec.Command("wtype", "--", text).Run()
		}
	}
	for _, r := range text {
		code, shift, _ := asciiKey(r)
		if err := d.tap(code, shift); err != nil {
			return err
		}
	}
	return nil
}

// syncInput makes the virtual device while Remote is open and removes it after.
func (s *server) syncInput() {
	want := s.watching("remote")
	s.mu.Lock()
	defer s.mu.Unlock()
	if want == (s.input != nil) {
		return
	}
	if want {
		d, err := openInput()
		if err != nil {
			log.Printf("uinput: %v", err)
			return
		}
		s.input = d
		time.Sleep(50 * time.Millisecond) // the compositor needs a moment to pick up the new device
		return
	}
	s.input.close()
	s.input = nil
}

// remote handles the Remote screen's input.
func (s *server) remote(p *session, m msg) {
	s.mu.Lock()
	d := s.input
	s.mu.Unlock()
	if d == nil {
		p.send(msg{T: "toast", Msg: "The PC can't use a virtual mouse right now."})
		return
	}
	var err error
	switch m.T {
	case "m":
		err = d.emit([3]int32{evRel, relX, int32(m.X)}, [3]int32{evRel, relY, int32(m.Y)})
	case "scroll":
		err = d.emit([3]int32{evRel, relWheel, int32(m.Y)})
	case "btn":
		if code, ok := map[string]uint16{"left": btnLeft, "right": btnRight, "middle": btnMid}[m.What]; ok {
			err = d.press(code, m.On)
		}
	case "key":
		if code, ok := namedKeys[m.What]; ok {
			err = d.tap(code, false)
		}
	case "type":
		if err = d.typeText(m.Text); err != nil {
			p.send(msg{T: "toast", Msg: err.Error()})
		}
	}
	if err != nil {
		log.Printf("remote %s: %v", m.T, err)
	}
}
