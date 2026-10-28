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

func (s *server) control(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return
	}
	enc := json.NewEncoder(c)
	switch strings.TrimSpace(line) {
	case "pair":
		uri, exp, err := s.newPairing()
		if err != nil {
			enc.Encode(map[string]string{"error": err.Error()})
			return
		}
		enc.Encode(map[string]any{"uri": uri, "expires": exp.Unix()})
	case "status":
		enc.Encode(s.snapshot())
	default:
		enc.Encode(map[string]string{"error": "unknown command"})
	}
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
