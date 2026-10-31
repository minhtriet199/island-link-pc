package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The Terminal screen: the phone opens ONE visible kitty on the PC, types in it and mirrors its
// screen. Off until the owner allows it in Island. Only the socket below can steer that window
// (allow_remote_control=socket-only, never globally), and the socket lives in the 0700 run dir.

const termPoll = 400 * time.Millisecond

type terminal struct {
	cmd  *exec.Cmd
	sock string
	done chan struct{}
	last string // the screen as last pushed, under server.mu
}

func (t *terminal) kitten(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kitten", append([]string{"@", "--to", "unix:" + t.sock}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
}

func (s *server) termMessage(p *session, m msg) {
	switch {
	case m.What == "open":
		s.openTerm(p)
	case m.What == "close":
		s.closeTerm()
	case m.Text != "":
		s.mu.Lock()
		t := s.term
		s.mu.Unlock()
		if t != nil {
			if _, err := t.kitten(m.Text, "send-text", "--stdin"); err != nil {
				log.Printf("send-text: %v", err)
			}
			s.pushTerm(t) // show the echo right away
		}
	}
}

func (s *server) openTerm(p *session) {
	s.mu.Lock()
	allowed, cur := s.cfg.Terminal, s.term
	s.mu.Unlock()
	switch {
	case !allowed:
		p.send(msg{T: "toast", Msg: "Turn on Terminal in Island's Phone Link first."})
		return
	case cur != nil:
		s.sendTerm(p)
		return
	}
	sock := filepath.Join(runDir(), "kitty.sock")
	os.Remove(sock)
	home, _ := os.UserHomeDir()
	cmd := exec.Command("kitty", "-o", "allow_remote_control=socket-only", "--listen-on", "unix:"+sock,
		"--title", "Phone terminal", "--directory", home)
	if err := cmd.Start(); err != nil {
		log.Printf("kitty: %v", err)
		p.send(msg{T: "toast", Msg: "The PC couldn't open kitty."})
		return
	}
	t := &terminal{cmd: cmd, sock: sock, done: make(chan struct{})}
	s.mu.Lock()
	s.term = t
	s.mu.Unlock()
	s.stateChanged()
	go func() {
		cmd.Wait()
		close(t.done)
		os.Remove(sock)
		s.mu.Lock()
		if s.term == t {
			s.term = nil
		}
		s.mu.Unlock()
		s.stateChanged()
		s.broadcast(map[string]any{"t": "term", "on": false, "text": ""})
	}()
	go s.mirror(t)
}

// mirror sends the window's screen to the phone, only while the Terminal screen is open and only on change.
func (s *server) mirror(t *terminal) {
	tick := time.NewTicker(termPoll)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
			if s.watching("term") {
				s.pushTerm(t)
			}
		}
	}
}

// pushTerm sends the screen if it changed since the last push.
func (s *server) pushTerm(t *terminal) {
	text, err := t.kitten("", "get-text")
	if err != nil {
		return
	}
	s.mu.Lock()
	same := t.last == text
	t.last = text
	s.mu.Unlock()
	if !same {
		s.broadcast(map[string]any{"t": "term", "on": true, "text": text})
	}
}

// sendTerm answers a Terminal screen that just opened with the current state.
func (s *server) sendTerm(p *session) {
	s.mu.Lock()
	t := s.term
	s.mu.Unlock()
	if t == nil {
		p.send(map[string]any{"t": "term", "on": false, "text": ""})
		return
	}
	text, _ := t.kitten("", "get-text")
	p.send(map[string]any{"t": "term", "on": true, "text": text})
}

func (s *server) closeTerm() {
	s.mu.Lock()
	t := s.term
	s.mu.Unlock()
	if t != nil {
		t.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
			t.cmd.Process.Kill()
		}
	}
}
