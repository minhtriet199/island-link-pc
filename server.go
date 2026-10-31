package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Wire format: one JSON object per line over TLS, "t" says what it is.
//
//	phone → pc  {"t":"pair","token","id","key","name"}  first time, token from the QR
//	phone → pc  {"t":"hello","id","key"}                every later connect
//	pc → phone  {"t":"welcome","pc","terminal"}  or  {"t":"error","msg"}
//	both ways   {"t":"ping"} → {"t":"pong"}  the PC starts it: a packet wakes a dozing phone, a timer in it would not
//	phone → pc  {"t":"file","name","size"} + size raw bytes   pc → phone {"t":"saved","name"}
//
// Everything else on the main link (features.go): notifications, battery, controls, remote input,
// clipboard, terminal. File transfers use their own connection (hello with "purpose":"files") so a big
// upload never delays the main link.
type msg struct {
	T        string  `json:"t"`
	Token    string  `json:"token,omitempty"`
	ID       string  `json:"id,omitempty"`
	Key      string  `json:"key,omitempty"`
	Name     string  `json:"name,omitempty"`
	PC       string  `json:"pc,omitempty"`
	Msg      string  `json:"msg,omitempty"`
	Purpose  string  `json:"purpose,omitempty"`
	Size     int64   `json:"size,omitempty"`
	Terminal bool    `json:"terminal,omitempty"`
	What     string  `json:"what,omitempty"`
	Arg      string  `json:"arg,omitempty"`
	On       bool    `json:"on,omitempty"`
	App      string  `json:"app,omitempty"`
	Title    string  `json:"title,omitempty"`
	Text     string  `json:"text,omitempty"`
	Level    int     `json:"level,omitempty"`
	Charging bool    `json:"charging,omitempty"`
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
}

const (
	pairTTL      = 3 * time.Minute  // how long a pairing QR works
	idleTimeout  = 75 * time.Second // the phone answers every heartbeat; two misses = gone
	heartbeat    = 30 * time.Second
	writeTimeout = 10 * time.Second
	maxLine      = 256 << 10
)

// server holds the live link state. One mutex: the traffic is tiny.
type server struct {
	mu       sync.Mutex
	pcName   string
	fp       string
	phones   map[string]phone
	online   map[string]*session
	token    string
	tokenAt  time.Time
	cfg      config
	islands  map[*islandConn]bool // Island's open control connections
	download json.RawMessage      // latest Download mode state from Island
	input    *inputDevice         // virtual mouse + keyboard, only while Remote is open
	music    *exec.Cmd            // playerctl --follow, only while Music is open
	term     *terminal            // the phone's kitty window, if any
}

// session is one phone's main link. Writes come from several goroutines, so they go through send.
type session struct {
	conn    net.Conn
	since   int64
	mu      sync.Mutex
	enc     *json.Encoder
	id      string
	name    string
	battery int // percent, -1 = not told yet
	charge  bool
	watch   map[string]bool // screens open on the phone that want live data
}

func newSession(c net.Conn, id, name string) *session {
	return &session{conn: c, since: time.Now().Unix(), enc: json.NewEncoder(c), id: id, name: name, battery: -1, watch: map[string]bool{}}
}

func (p *session) send(v any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return p.enc.Encode(v)
}

func newServer(name, fp string) *server {
	return &server{pcName: name, fp: fp, phones: loadPhones(), online: map[string]*session{},
		cfg: loadConfig(), islands: map[*islandConn]bool{}}
}

func serve() error {
	cert, fp, err := loadIdentity()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	host, _ := os.Hostname()
	s := newServer(host, fp)

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
	s.stateChanged()

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
	r := bufio.NewReaderSize(c, maxLine)
	enc := json.NewEncoder(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	var first msg
	if readMsg(r, &first) != nil || first.ID == "" || first.Key == "" {
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
		first.Name = p.Name
	default:
		return
	}

	s.mu.Lock()
	terminal := s.cfg.Terminal
	s.mu.Unlock()
	if enc.Encode(msg{T: "welcome", PC: s.pcName, Terminal: terminal}) != nil {
		return
	}
	sess := newSession(c, first.ID, first.Name)
	if first.Purpose != "files" {
		s.markOnline(sess)
		defer s.markOffline(sess)
		stop := make(chan struct{})
		defer close(stop)
		go sess.heartbeat(stop)
		s.greet(sess)
	}

	for {
		c.SetReadDeadline(time.Now().Add(idleTimeout))
		var m msg
		if err := readMsg(r, &m); err != nil {
			if errors.Is(err, errBadMsg) {
				continue
			}
			return
		}
		switch m.T {
		case "ping":
			sess.send(msg{T: "pong"})
		case "pong":
		case "file":
			c.SetReadDeadline(time.Time{}) // big files take a while; the phone closes on failure
			name, err := receiveFile(r, m.Name, m.Size, first.Name)
			if err != nil {
				log.Printf("file %q: %v", m.Name, err)
				sess.send(msg{T: "error", Msg: "The PC couldn't save " + m.Name + "."})
				return
			}
			sess.send(msg{T: "saved", Name: name})
		default:
			if first.Purpose != "files" {
				s.feature(sess, m)
			}
		}
	}
}

// heartbeat keeps the link alive from this side, see the wire format note.
func (p *session) heartbeat(stop <-chan struct{}) {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.send(msg{T: "ping"})
		case <-stop:
			return
		}
	}
}

var errBadMsg = errors.New("not a message")

// readMsg reads one JSON line. A line longer than the buffer ends the connection.
func readMsg(r *bufio.Reader, m *msg) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	if json.Unmarshal(line, m) != nil {
		return errBadMsg
	}
	return nil
}

// markOnline marks a phone connected; a newer connection from the same phone replaces the old one.
func (s *server) markOnline(p *session) {
	s.mu.Lock()
	if old := s.online[p.id]; old != nil {
		old.conn.Close()
	}
	s.online[p.id] = p
	s.mu.Unlock()
	s.stateChanged()
}

func (s *server) markOffline(p *session) {
	s.mu.Lock()
	gone := s.online[p.id] == p
	if gone {
		delete(s.online, p.id)
	}
	s.mu.Unlock()
	if gone {
		s.watchersChanged()
		s.stateChanged()
	}
}

// state is what Island shows: it arrives on the control socket as {"ev":"state",...} on every change.
type state struct {
	PC       string       `json:"pc"`
	Port     int          `json:"port"`
	Pairing  bool         `json:"pairing"`
	Phones   []phoneState `json:"phones"`
	Terminal bool         `json:"terminal"` // the phone may open a terminal
	TermOpen bool         `json:"termOpen"`
}

type phoneState struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Since     int64  `json:"since,omitempty"`
	Battery   int    `json:"battery"`
	Charging  bool   `json:"charging,omitempty"`
}

func (s *server) snapshot() state {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := state{PC: s.pcName, Port: port, Pairing: s.token != "" && time.Since(s.tokenAt) < pairTTL, Phones: []phoneState{},
		Terminal: s.cfg.Terminal, TermOpen: s.term != nil}
	for id, p := range s.phones {
		ps := phoneState{ID: id, Name: p.Name, Battery: -1}
		if sess := s.online[id]; sess != nil {
			ps.Connected, ps.Since, ps.Battery, ps.Charging = true, sess.since, sess.battery, sess.charge
		}
		st.Phones = append(st.Phones, ps)
	}
	sort.Slice(st.Phones, func(i, j int) bool { return st.Phones[i].Name < st.Phones[j].Name })
	return st
}

// sessions is a copy of the phones online, so sends happen outside the lock.
func (s *server) sessions() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session, 0, len(s.online))
	for _, p := range s.online {
		out = append(out, p)
	}
	return out
}

func (s *server) broadcast(v any) {
	for _, p := range s.sessions() {
		p.send(v)
	}
}

// watching reports whether any phone has this screen open.
func (s *server) watching(what string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.online {
		if p.watch[what] {
			return true
		}
	}
	return false
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
