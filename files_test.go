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

	// same name: identical content is kept once, different content gets "(2)"
	for _, body := range []string{"hello", "hello", "other"} {
		enc.Encode(msg{T: "file", Name: "shot.png", Size: int64(len(body))})
		c.Write([]byte(body))
		reply(t, r)
	}
	for name, want := range map[string]string{"shot.png": "hello", "shot (2).png": "other"} {
		b, err := os.ReadFile(filepath.Join(inboxDir(), name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(inboxDir(), "shot (3).png")); err == nil {
		t.Fatal("the repeated file was saved again")
	}
	if left, _ := filepath.Glob(filepath.Join(inboxDir(), ".*.part")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
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
