package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Wire format: one JSON object per line over TLS, "t" says what it is.
//
//	phone → pc  {"t":"pair","token","id","key","name"}  first time, token from the QR
//	phone → pc  {"t":"hello","id","key"}                every later connect
//	pc → phone  {"t":"welcome","pc"}  or  {"t":"error","msg"}
//	phone → pc  {"t":"ping"}          pc → phone {"t":"pong"}
type msg struct {
	T     string `json:"t"`
	Token string `json:"token,omitempty"`
	ID    string `json:"id,omitempty"`
	Key   string `json:"key,omitempty"`
	Name  string `json:"name,omitempty"`
	PC    string `json:"pc,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

const (
	pairTTL     = 3 * time.Minute  // how long a pairing QR works
	idleTimeout = 75 * time.Second // phone pings every 25 s; three misses = gone
	maxLine     = 64 << 10
)

// server holds the live link state. One mutex: the traffic is tiny.
type server struct {
	mu      sync.Mutex
	pcName  string
	fp      string
	phones  map[string]phone
	online  map[string]*session
	token   string
	tokenAt time.Time
}

type session struct {
	conn  net.Conn
	since int64
}

func serve() error {
	cert, fp, err := loadIdentity()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	host, _ := os.Hostname()
	s := &server{pcName: host, fp: fp, phones: loadPhones(), online: map[string]*session{}}

	ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", port), &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		return err
	}
	if err := s.listenControl(); err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	s.writeState()
	log.Printf("listening on :%d (fingerprint %s…)", port, fp[:12])
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

// newPairing makes a fresh one-time token and returns the URI the QR carries.
func (s *server) newPairing() (string, time.Time, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	s.token, s.tokenAt = base64.RawURLEncoding.EncodeToString(b), time.Now()
	token := s.token
	s.mu.Unlock()
	s.writeState()

	q := url.Values{}
	q.Set("v", "1")
	q.Set("h", localIP())
	q.Set("p", fmt.Sprint(port))
	q.Set("fp", s.fp)
	q.Set("t", token)
	q.Set("n", s.pcName)
	return "islandlink://pair?" + q.Encode(), time.Now().Add(pairTTL), nil
}

// takeToken checks a pairing token and burns it, so each QR works once.
func (s *server) takeToken(t string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.token != "" && time.Since(s.tokenAt) < pairTTL &&
		subtle.ConstantTimeCompare([]byte(s.token), []byte(t)) == 1
	if ok {
		s.token = ""
	}
	return ok
}

func (s *server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewScanner(c)
	r.Buffer(make([]byte, 4096), maxLine)
	enc := json.NewEncoder(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if !r.Scan() {
		return
	}
	var first msg
	if json.Unmarshal(r.Bytes(), &first) != nil || first.ID == "" || first.Key == "" {
		return
	}

	switch first.T {
	case "pair":
		if !s.takeToken(first.Token) {
			enc.Encode(msg{T: "error", Msg: "This code has expired. Show a new one on your PC."})
			return
		}
		s.mu.Lock()
		s.phones[first.ID] = newPhone(first.ID, first.Name, first.Key)
		err := savePhones(s.phones)
		s.mu.Unlock()
		if err != nil {
			log.Printf("saving phones: %v", err)
			enc.Encode(msg{T: "error", Msg: "The PC could not save the pairing."})
			return
		}
		log.Printf("paired %q", first.Name)
	case "hello":
		s.mu.Lock()
		p, known := s.phones[first.ID]
		s.mu.Unlock()
		if !known || !p.keyMatches(first.Key) {
			enc.Encode(msg{T: "error", Msg: "unpaired"})
			return
		}
	default:
		return
	}

	if enc.Encode(msg{T: "welcome", PC: s.pcName}) != nil {
		return
	}
	s.markOnline(first.ID, c)
	defer s.markOffline(first.ID, c)

	for {
		c.SetReadDeadline(time.Now().Add(idleTimeout))
		if !r.Scan() {
			return
		}
		var m msg
		if json.Unmarshal(r.Bytes(), &m) != nil {
			continue
		}
		if m.T == "ping" {
			enc.Encode(msg{T: "pong"})
		}
	}
}

// markOnline marks a phone connected; a newer connection from the same phone replaces the old one.
func (s *server) markOnline(id string, c net.Conn) {
	s.mu.Lock()
	if old := s.online[id]; old != nil {
		old.conn.Close()
	}
	s.online[id] = &session{conn: c, since: time.Now().Unix()}
	s.mu.Unlock()
	s.writeState()
}

func (s *server) markOffline(id string, c net.Conn) {
	s.mu.Lock()
	if cur := s.online[id]; cur != nil && cur.conn == c {
		delete(s.online, id)
	}
	s.mu.Unlock()
	s.writeState()
}

// state is what Island reads from $XDG_RUNTIME_DIR/island-link/state.json.
type state struct {
	PC      string       `json:"pc"`
	Port    int          `json:"port"`
	Pairing bool         `json:"pairing"`
	Phones  []phoneState `json:"phones"`
}

type phoneState struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Since     int64  `json:"since,omitempty"`
}

func (s *server) snapshot() state {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := state{PC: s.pcName, Port: port, Pairing: s.token != "" && time.Since(s.tokenAt) < pairTTL, Phones: []phoneState{}}
	for id, p := range s.phones {
		ps := phoneState{ID: id, Name: p.Name}
		if sess := s.online[id]; sess != nil {
			ps.Connected, ps.Since = true, sess.since
		}
		st.Phones = append(st.Phones, ps)
	}
	sort.Slice(st.Phones, func(i, j int) bool { return st.Phones[i].Name < st.Phones[j].Name })
	return st
}

func (s *server) writeState() {
	b, _ := json.Marshal(s.snapshot())
	if err := writeAtomic(filepath.Join(runDir(), "state.json"), b, 0o600); err != nil {
		log.Printf("state: %v", err)
	}
}

// localIP is the address of the interface with the default route (no packet is sent).
func localIP() string {
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
