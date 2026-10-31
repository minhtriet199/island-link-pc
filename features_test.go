package main

import (
	"bufio"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// online pairs a fake phone and returns its connection.
func online(t *testing.T, s *server) (*json.Encoder, *bufio.Scanner) {
	t.Helper()
	s.phones["p"] = newPhone("p", "Pixel", "k")
	_, cfg := testTLS(t, s)
	enc, r, _ := dial(t, s, cfg)
	enc.Encode(msg{T: "hello", ID: "p", Key: "k"})
	if m := reply(t, r); m.T != "welcome" {
		t.Fatalf("hello: %+v", m)
	}
	return enc, r
}

// spawned records what would have run instead of running it.
func spawned(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	old := spawn
	spawn = func(name string, args ...string) {
		mu.Lock()
		calls = append(calls, append([]string{name}, args...))
		mu.Unlock()
	}
	t.Cleanup(func() { spawn = old })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200 && !cond(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("timed out")
	}
}

// island connects like Island does and returns its events.
func island(t *testing.T, s *server) <-chan map[string]any {
	t.Helper()
	a, b := net.Pipe()
	go s.control(a)
	t.Cleanup(func() { b.Close() })
	b.Write([]byte("subscribe\n"))
	events := make(chan map[string]any, 16)
	go func() {
		dec := json.NewDecoder(b)
		for {
			var ev map[string]any
			if dec.Decode(&ev) != nil {
				return
			}
			events <- ev
		}
	}()
	return events
}

func TestPhoneNotificationBecomesBubble(t *testing.T) {
	s, _ := testServer(t)
	calls := spawned(t)
	enc, _ := online(t, s)
	enc.Encode(msg{T: "notif", App: "Messages", Title: "An", Text: "--help"})
	waitFor(t, func() bool { return len(calls()) == 1 })
	want := []string{"notify-send", "-a", "Messages", "-i", "phone", "-h", "string:desktop-entry:island-link", "--", "An", "--help"}
	if !reflect.DeepEqual(calls()[0], want) {
		t.Fatalf("got %q", calls()[0])
	}
}

func TestOnlyNamedActionsRun(t *testing.T) {
	s, _ := testServer(t)
	calls := spawned(t)
	enc, r := online(t, s)
	enc.Encode(msg{T: "do", What: "rm -rf /"})
	enc.Encode(msg{T: "do", What: "next"})
	waitFor(t, func() bool { return len(calls()) == 1 })
	if got := calls()[0]; got[0] != "playerctl" || got[1] != "next" {
		t.Fatalf("got %q", got)
	}
	// Power goes through Island; without Island the phone is told so.
	enc.Encode(msg{T: "do", What: "shutdown"})
	if m := reply(t, r); m.T != "toast" {
		t.Fatalf("got %+v", m)
	}
}

func TestPowerGoesThroughIsland(t *testing.T) {
	s, _ := testServer(t)
	events := island(t, s)
	<-events // the first state
	enc, _ := online(t, s)
	enc.Encode(msg{T: "do", What: "lock"})
	for ev := range events {
		if ev["ev"] == "do" {
			if ev["what"] != "lock" {
				t.Fatalf("got %v", ev)
			}
			return
		}
	}
}

func TestDownloadStateReachesThePhone(t *testing.T) {
	s, _ := testServer(t)
	_, r := online(t, s)
	go s.setDownload(`{"on":true,"speed":5}`)
	if !r.Scan() || !strings.Contains(r.Text(), `"on":true`) || !strings.Contains(r.Text(), `"t":"dl"`) {
		t.Fatalf("got %s", r.Text())
	}
}

func TestLateArrivingPhoneGetsDownloadState(t *testing.T) {
	s, _ := testServer(t)
	s.setDownload(`{"on":true}`)
	_, r := online(t, s)
	if !r.Scan() || !strings.Contains(r.Text(), `"t":"dl"`) {
		t.Fatalf("got %s", r.Text())
	}
}

func TestForgetDropsThePhone(t *testing.T) {
	s, _ := testServer(t)
	_, r := online(t, s)
	go s.forget("p")
	if m := reply(t, r); m.T != "error" || m.Msg != "unpaired" {
		t.Fatalf("got %+v", m)
	}
	if _, ok := loadPhones()["p"]; ok {
		t.Fatal("still saved")
	}
}

func TestTerminalIsOffByDefault(t *testing.T) {
	s, _ := testServer(t)
	enc, r := online(t, s)
	enc.Encode(msg{T: "term", What: "open"})
	if m := reply(t, r); m.T != "toast" {
		t.Fatalf("got %+v", m)
	}
	if s.term != nil {
		t.Fatal("kitty must not open while the owner hasn't allowed it")
	}
}

func TestTerminalChoiceSurvivesRestart(t *testing.T) {
	s, _ := testServer(t)
	s.setTerminal(true)
	if !loadConfig().Terminal {
		t.Fatal("not saved")
	}
}

func TestWatchTracksScreens(t *testing.T) {
	s, _ := testServer(t)
	enc, _ := online(t, s)
	enc.Encode(msg{T: "watch", What: "nothing-real", On: true})
	waitFor(t, func() bool { return s.watching("nothing-real") })
}

func TestAsciiKeysFollowTheUSLayout(t *testing.T) {
	for r, want := range map[rune]struct {
		code  uint16
		shift bool
	}{'a': {30, false}, 'Z': {44, true}, '1': {2, false}, '!': {2, true}, ' ': {57, false}, '\n': {28, false},
		'/': {53, false}, '?': {53, true}, '|': {43, true}, '"': {40, true}, '~': {41, true}} {
		code, shift, ok := asciiKey(r)
		if !ok || code != want.code || shift != want.shift {
			t.Errorf("%q: got %d %v %v", r, code, shift, ok)
		}
	}
	if _, _, ok := asciiKey('ă'); ok {
		t.Error("non-ASCII must not map to a key")
	}
}
