package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The control socket lets Island (and the CLI) ask the running link for things.
// One request line in, one JSON line out. Only this user can open it (0700 dir).
func controlPath() string { return filepath.Join(runDir(), "ctl.sock") }

func (s *server) listenControl() error {
	if err := os.MkdirAll(runDir(), 0o700); err != nil {
		return err
	}
	os.Remove(controlPath())
	ln, err := net.Listen("unix", controlPath())
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.control(c)
		}
	}()
	return nil
}

// islandConn is one open control connection. After "subscribe" it also gets {"ev":...} lines.
type islandConn struct {
	conn net.Conn
	mu   sync.Mutex
	enc  *json.Encoder
}

func (c *islandConn) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.enc.Encode(v)
}

// control serves one client. The CLI sends one line; Island keeps the connection and sends many.
func (s *server) control(c net.Conn) {
	defer c.Close()
	ic := &islandConn{conn: c, enc: json.NewEncoder(c)}
	defer func() {
		s.mu.Lock()
		delete(s.islands, ic)
		s.mu.Unlock()
	}()
	r := bufio.NewReaderSize(c, maxLine)
	for {
		s.mu.Lock()
		subscribed := s.islands[ic]
		s.mu.Unlock()
		if subscribed {
			c.SetReadDeadline(time.Time{})
		} else {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch cmd {
		case "subscribe":
			s.mu.Lock()
			s.islands[ic] = true
			s.mu.Unlock()
			ic.send(s.stateEvent())
		case "pair":
			uri, exp, err := s.newPairing()
			if err != nil {
				ic.send(map[string]any{"ev": "pair", "error": err.Error()})
				continue
			}
			ic.send(map[string]any{"ev": "pair", "uri": uri, "expires": exp.Unix(), "qr": writeQR(uri)})
		case "status":
			ic.send(s.snapshot())
		case "dl": // Download mode state from Island, passed on to the phones as it is
			if json.Valid([]byte(arg)) {
				s.setDownload(arg)
			}
		case "forget":
			s.forget(arg)
		case "terminal": // "terminal 1" / "terminal 0": may the phone open a terminal?
			s.setTerminal(arg == "1")
		case "term-close":
			s.closeTerm()
		default:
			ic.send(map[string]string{"ev": "error", "error": "unknown command"})
		}
	}
}

// stateChanged tells Island.
func (s *server) stateChanged() { s.toIsland(s.stateEvent()) }

func (s *server) stateEvent() any {
	return struct {
		Ev string `json:"ev"`
		state
	}{"state", s.snapshot()}
}

// toIsland sends an event to every open Island; false = Island isn't running.
func (s *server) toIsland(v any) bool {
	s.mu.Lock()
	list := make([]*islandConn, 0, len(s.islands))
	for c := range s.islands {
		list = append(list, c)
	}
	s.mu.Unlock()
	for _, c := range list {
		c.send(v)
	}
	return len(list) > 0
}

// writeQR renders the pairing QR for Island; "" when qrencode is missing.
func writeQR(uri string) string {
	path := filepath.Join(runDir(), "pair-qr.png")
	if exec.Command("qrencode", "-o", path, "-s", "10", "-m", "2", uri).Run() != nil {
		return ""
	}
	return path
}

// ask sends one command to the running link and decodes the reply into out.
func ask(cmd string, out any) error {
	c, err := net.DialTimeout("unix", controlPath(), 2*time.Second)
	if err != nil {
		return errors.New("the link is not running (start it with: island-link-pc serve)")
	}
	defer c.Close()
	fmt.Fprintln(c, cmd)
	return json.NewDecoder(c).Decode(out)
}

func pairCmd(uriOnly bool) error {
	var r struct {
		URI     string `json:"uri"`
		Expires int64  `json:"expires"`
		Error   string `json:"error"`
	}
	if err := ask("pair", &r); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if uriOnly {
		fmt.Println(r.URI)
		return nil
	}
	// Terminal QR via qrencode when it's there (Island renders its own PNG).
	if q, err := exec.LookPath("qrencode"); err == nil {
		cmd := exec.Command(q, "-t", "ansiutf8", "-m", "2", r.URI)
		cmd.Stdout = os.Stdout
		cmd.Run()
	}
	fmt.Printf("Scan this with Island Link on your phone (works once, until %s).\n%s\n",
		time.Unix(r.Expires, 0).Format("15:04"), r.URI)
	return nil
}

func statusCmd() error {
	var st state
	if err := ask("status", &st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(b))
	return nil
}
