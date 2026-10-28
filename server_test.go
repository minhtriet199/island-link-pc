package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"testing"
)

// testServer runs a server with its files in temp dirs.
func testServer(t *testing.T) (*server, *tls.Config) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cert, fp, err := loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := &server{pcName: "TestPC", fp: fp, phones: loadPhones(), online: map[string]*session{}}
	return s, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

// dial connects like the phone does: trust only the pinned fingerprint.
func dial(t *testing.T, s *server, cfg *tls.Config) (*json.Encoder, *bufio.Scanner, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	go s.handle(tls.Server(a, cfg))
	c := tls.Client(b, &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin check below, as on the phone
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != s.fp {
				t.Fatal("fingerprint mismatch")
			}
			return nil
		},
	})
	t.Cleanup(func() { b.Close() }) // the raw pipe: a TLS close would block on close-notify
	return json.NewEncoder(c), bufio.NewScanner(c), c
}

func reply(t *testing.T, r *bufio.Scanner) msg {
	t.Helper()
	if !r.Scan() {
		t.Fatal("connection closed")
	}
	var m msg
	if err := json.Unmarshal(r.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPairThenHello(t *testing.T) {
	s, cfg := testServer(t)
	uri, _, err := s.newPairing()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	if u.Query().Get("fp") != s.fp {
		t.Fatal("QR must carry the fingerprint")
	}

	enc, r, _ := dial(t, s, cfg)
	enc.Encode(msg{T: "pair", Token: u.Query().Get("t"), ID: "phone1", Key: "secret", Name: "Pixel 9"})
	if m := reply(t, r); m.T != "welcome" || m.PC != "TestPC" {
		t.Fatalf("pair: got %+v", m)
	}
	enc.Encode(msg{T: "ping"})
	if m := reply(t, r); m.T != "pong" {
		t.Fatalf("ping: got %+v", m)
	}
	if st := s.snapshot(); len(st.Phones) != 1 || !st.Phones[0].Connected {
		t.Fatalf("state: %+v", st)
	}

	// Pairing survives a restart: a fresh load sees the phone, and hello works.
	if _, ok := loadPhones()["phone1"]; !ok {
		t.Fatal("phone not saved")
	}
	enc2, r2, _ := dial(t, s, cfg)
	enc2.Encode(msg{T: "hello", ID: "phone1", Key: "secret"})
	if m := reply(t, r2); m.T != "welcome" {
		t.Fatalf("hello: got %+v", m)
	}
}

func TestTokenWorksOnce(t *testing.T) {
	s, cfg := testServer(t)
	uri, _, _ := s.newPairing()
	u, _ := url.Parse(uri)
	tok := u.Query().Get("t")

	enc, r, _ := dial(t, s, cfg)
	enc.Encode(msg{T: "pair", Token: tok, ID: "a", Key: "k", Name: "A"})
	reply(t, r)

	enc2, r2, _ := dial(t, s, cfg)
	enc2.Encode(msg{T: "pair", Token: tok, ID: "b", Key: "k", Name: "B"})
	if m := reply(t, r2); m.T != "error" {
		t.Fatalf("reused token must fail, got %+v", m)
	}
}

func TestHelloRejectsWrongKey(t *testing.T) {
	s, cfg := testServer(t)
	s.phones["p"] = newPhone("p", "P", "right")

	enc, r, _ := dial(t, s, cfg)
	enc.Encode(msg{T: "hello", ID: "p", Key: "wrong"})
	if m := reply(t, r); m.T != "error" || m.Msg != "unpaired" {
		t.Fatalf("got %+v", m)
	}
}
