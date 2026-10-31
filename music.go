package main

import (
	"bufio"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// syncMusic runs `playerctl --follow` only while the Music screen is open.
func (s *server) syncMusic() {
	want := s.watching("music")
	s.mu.Lock()
	cur := s.music
	if want == (cur != nil) {
		s.mu.Unlock()
		return
	}
	if !want {
		s.music = nil
		s.mu.Unlock()
		cur.Process.Kill()
		return
	}
	cmd := exec.Command("playerctl", "--follow", "metadata", "--format",
		"{{status}}\t{{artist}}\t{{title}}\t{{playerName}}")
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		s.mu.Unlock()
		log.Printf("playerctl: %v", err)
		return
	}
	s.music = cmd
	s.mu.Unlock()
	go func() {
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			s.broadcastMusic(lines.Text())
		}
		cmd.Wait()
		s.mu.Lock()
		if s.music == cmd { // it died by itself: the next open starts a new one
			s.music = nil
		}
		s.mu.Unlock()
	}()
}

func (s *server) broadcastMusic(line string) {
	f := strings.SplitN(line, "\t", 4)
	for len(f) < 4 {
		f = append(f, "")
	}
	for _, p := range s.sessions() {
		s.mu.Lock()
		wants := p.watch["music"]
		s.mu.Unlock()
		if wants {
			p.send(map[string]any{"t": "music", "status": f[0], "artist": f[1], "title": f[2], "player": f[3]})
		}
	}
}

var wpVolume = regexp.MustCompile(`Volume: ([0-9.]+)( \[MUTED\])?`)

// sendLevels reports volume and screen brightness to the phone's sliders.
func (s *server) sendLevels(p *session) {
	l := map[string]any{"t": "levels", "vol": -1, "muted": false, "bright": -1}
	if out, err := exec.Command("wpctl", "get-volume", "@DEFAULT_AUDIO_SINK@").Output(); err == nil {
		if m := wpVolume.FindStringSubmatch(string(out)); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			l["vol"], l["muted"] = int(v*100+0.5), m[2] != ""
		}
	}
	if out, err := exec.Command("brightnessctl", "-m").Output(); err == nil {
		if f := strings.Split(strings.TrimSpace(string(out)), ","); len(f) > 3 {
			if n, err := strconv.Atoi(strings.TrimSuffix(f[3], "%")); err == nil {
				l["bright"] = n
			}
		}
	}
	p.send(l)
}
