package main

import (
	"bufio"
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
	final := freeName(dir, cleanName(name))
	part := filepath.Join(dir, "."+final+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	_, err = io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, filepath.Join(dir, final))
	}
	if err != nil {
		os.Remove(part)
		return "", err
	}
	notifyNewFile(from, final)
	return final, nil
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
