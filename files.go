package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxFileSize caps one incoming file (a phone video can be big, a runaway sender can't fill the disk).
const maxFileSize = 8 << 30

// inboxDir is where files from the phone land: ~/Downloads/Island Link.
func inboxDir() string {
	dl := os.Getenv("XDG_DOWNLOAD_DIR")
	if dl == "" {
		home, _ := os.UserHomeDir()
		dl = filepath.Join(home, "Downloads")
	}
	return filepath.Join(dl, "Island Link")
}

// receiveFile copies exactly size bytes from r into the inbox and returns the name it was saved as.
// It writes to a .part file first, so a cut-off transfer never looks like a finished file.
func receiveFile(r *bufio.Reader, name string, size int64, from string) (string, error) {
	if size < 0 || size > maxFileSize {
		return "", fmt.Errorf("bad size %d", size)
	}
	dir := inboxDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := cleanName(name)
	f, err := os.CreateTemp(dir, "."+base+".*.part")
	if err != nil {
		return "", err
	}
	part := f.Name()
	_, err = io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return "", err
	}
	// the same file sent again (e.g. shared twice from the phone): keep the one already here, no " (2)" copy
	if same := sameFile(dir, base, part); same != "" {
		os.Remove(part)
		return same, nil
	}
	final := freeName(dir, base)
	if err := os.Rename(part, filepath.Join(dir, final)); err != nil {
		os.Remove(part)
		return "", err
	}
	notifyNewFile(from, final)
	return final, nil
}

// sameFile is the name of a file already in dir, "name" or "name (N)", with exactly part's content, or "".
func sameFile(dir, name, part string) string {
	want, err := os.Stat(part)
	if err != nil {
		return ""
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		st, err := os.Lstat(filepath.Join(dir, try))
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err == nil && st.Mode().IsRegular() && st.Size() == want.Size() && sameBytes(filepath.Join(dir, try), part) {
			return try
		}
	}
}

func sameBytes(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil || eb != nil { // same size (checked first), so both end together
			return ea != nil && eb != nil
		}
	}
}

// cleanName keeps only the base name and drops characters that cause trouble in paths.
func cleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		return "file"
	}
	return name
}

// freeName returns name, or "name (2).ext", "name (3).ext"… if it's taken.
func freeName(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		if _, err := os.Lstat(filepath.Join(dir, try)); errors.Is(err, os.ErrNotExist) {
			return try
		}
	}
}

// notifyNewFile is swapped out in tests so they never pop bubbles on the real desktop.
var notifyNewFile = func(from, name string) {
	if from == "" {
		from = "your phone"
	}
	spawn("notify-send", "-a", "Island Link", "-i", "folder-download", "--", "From "+from, name)
}
