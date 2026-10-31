package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileTransfer(t *testing.T) {
	s, cfg := testServer(t)
	t.Setenv("XDG_DOWNLOAD_DIR", t.TempDir())
	var bubbles []string
	old := notifyNewFile
	notifyNewFile = func(from, name string) { bubbles = append(bubbles, from+": "+name) }
	t.Cleanup(func() { notifyNewFile = old })
	s.phones["p"] = newPhone("p", "Pixel 9", "k")

	enc, r, c := dial(t, s, cfg)
	enc.Encode(msg{T: "hello", ID: "p", Key: "k", Purpose: "files"})
	if m := reply(t, r); m.T != "welcome" {
		t.Fatalf("hello: %+v", m)
	}
	if st := s.snapshot(); st.Phones[0].Connected {
		t.Fatal("a file connection must not replace the phone's main session")
	}

	for range 2 { // same name twice → the second gets "(2)"
		enc.Encode(msg{T: "file", Name: "shot.png", Size: 5})
		c.Write([]byte("hello"))
		reply(t, r)
	}
	for _, name := range []string{"shot.png", "shot (2).png"} {
		b, err := os.ReadFile(filepath.Join(inboxDir(), name))
		if err != nil || string(b) != "hello" {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if len(bubbles) != 2 || bubbles[0] != "Pixel 9: shot.png" {
		t.Fatalf("bubbles: %v", bubbles)
	}
}

func TestCleanNameStaysInInbox(t *testing.T) {
	for in, want := range map[string]string{
		"../../.bashrc":    "bashrc",
		`..\..\evil.exe`:   "evil.exe",
		"/etc/passwd":      "passwd",
		"":                 "file",
		"a\x00b.txt":       "ab.txt",
		"Screenshot 1.png": "Screenshot 1.png",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
