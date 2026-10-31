package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// phone is one paired phone. Only a hash of its key is kept on disk.
type phone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	KeyHash string `json:"key_hash"`
	Paired  int64  `json:"paired"`
}

// config holds the owner's choices. The terminal stays off until they turn it on in Island.
type config struct {
	Terminal bool `json:"terminal"`
}

func configFile() string { return filepath.Join(dataDir(), "config.json") }

func loadConfig() config {
	var c config
	if b, err := os.ReadFile(configFile()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(configFile(), b, 0o600)
}

func phonesFile() string { return filepath.Join(dataDir(), "phones.json") }

func loadPhones() map[string]phone {
	out := map[string]phone{}
	b, err := os.ReadFile(phonesFile())
	if err != nil {
		return out
	}
	var list []phone
	if json.Unmarshal(b, &list) == nil {
		for _, p := range list {
			out[p.ID] = p
		}
	}
	return out
}

func savePhones(phones map[string]phone) error {
	list := make([]phone, 0, len(phones))
	for _, p := range phones {
		list = append(list, p)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return writeAtomic(phonesFile(), b, 0o600)
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (p phone) keyMatches(key string) bool {
	return subtle.ConstantTimeCompare([]byte(p.KeyHash), []byte(hashKey(key))) == 1
}

func newPhone(id, name, key string) phone {
	return phone{ID: id, Name: name, KeyHash: hashKey(key), Paired: time.Now().Unix()}
}

// writeAtomic writes via a temp file + rename so readers (Island) never see half a file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
