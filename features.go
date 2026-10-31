package main

import (
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Main-link messages beyond the basics. Phone → PC:
//
//	{"t":"notif","app","title","text"}   a phone notification → Island bubble
//	{"t":"battery","level","charging"}
//	{"t":"do","what","arg"}              one allowed action, see do()
//	{"t":"watch","what","on"}            a screen opened / closed: "music", "term", "remote"
//	{"t":"m","x","y"} {"t":"scroll","y"} {"t":"btn","what","on"} {"t":"key","what"} {"t":"type","text"}
//	{"t":"clip","text"}  {"t":"clipget"}
//	{"t":"term","what":"open|close"}  {"t":"term","text"}
//
// PC → phone: {"t":"dl","s":{…}} {"t":"music",…} {"t":"levels",…} {"t":"clip","text"}
// {"t":"term","on","text"} {"t":"config","terminal"} {"t":"toast","msg"}
func (s *server) feature(p *session, m msg) {
	switch m.T {
	case "notif":
		showPhoneNotif(m.App, m.Title, m.Text)
	case "battery":
		s.mu.Lock()
		p.battery, p.charge = m.Level, m.Charging
		s.mu.Unlock()
		s.stateChanged()
	case "do":
		s.do(p, m.What, m.Arg)
	case "watch":
		s.mu.Lock()
		p.watch[m.What] = m.On
		s.mu.Unlock()
		s.watchersChanged()
		if m.What == "term" && m.On {
			s.sendTerm(p)
		}
	case "m", "scroll", "btn", "key", "type":
		s.remote(p, m)
	case "clip":
		if len(m.Text) > 0 {
			cmd := exec.Command("wl-copy")
			cmd.Stdin = strings.NewReader(m.Text)
			if err := cmd.Run(); err != nil {
				log.Printf("wl-copy: %v", err)
				p.send(msg{T: "toast", Msg: "The PC couldn't copy that."})
			}
		}
	case "clipget":
		p.send(msg{T: "clip", Text: pcClipboard()})
	case "term":
		s.termMessage(p, m)
	}
}

// watchersChanged starts or stops the things that only run while a phone screen needs them.
func (s *server) watchersChanged() {
	s.syncMusic()
	s.syncInput()
}

// greet sends a phone what it missed while away.
func (s *server) greet(p *session) {
	s.mu.Lock()
	d := s.download
	s.mu.Unlock()
	if d != nil {
		p.send(map[string]any{"t": "dl", "s": d})
	}
}

// setDownload takes Download mode's state from Island and passes it on to the phones.
func (s *server) setDownload(raw string) {
	s.mu.Lock()
	s.download = json.RawMessage(raw)
	s.mu.Unlock()
	s.broadcast(map[string]any{"t": "dl", "s": json.RawMessage(raw)})
}

func (s *server) forget(id string) {
	s.mu.Lock()
	delete(s.phones, id)
	err := savePhones(s.phones)
	p := s.online[id]
	s.mu.Unlock()
	if err != nil {
		log.Printf("saving phones: %v", err)
	}
	if p != nil {
		p.send(msg{T: "error", Msg: "unpaired"})
		p.conn.Close()
	}
	s.stateChanged()
}

func (s *server) setTerminal(on bool) {
	s.mu.Lock()
	s.cfg.Terminal = on
	err := saveConfig(s.cfg)
	s.mu.Unlock()
	if err != nil {
		log.Printf("saving config: %v", err)
	}
	if !on {
		s.closeTerm()
	}
	s.broadcast(msg{T: "config", Terminal: on})
	s.stateChanged()
}

var percent = regexp.MustCompile(`^\d{1,3}$`)

// do runs one action from the phone. Only what is named here can run; arguments are checked.
func (s *server) do(p *session, what, arg string) {
	switch what {
	case "play-pause", "next", "previous":
		spawn("playerctl", what)
	case "volume":
		if percent.MatchString(arg) {
			exec.Command("wpctl", "set-volume", "-l", "1", "@DEFAULT_AUDIO_SINK@", arg+"%").Run()
			s.sendLevels(p)
		}
	case "mute":
		exec.Command("wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", "toggle").Run()
		s.sendLevels(p)
	case "brightness":
		if n, err := strconv.Atoi(arg); err == nil && n >= 2 && n <= 100 {
			exec.Command("brightnessctl", "set", arg+"%").Run()
			s.sendLevels(p)
		}
	case "levels":
		s.sendLevels(p)
	case "lock", "suspend", "reboot", "shutdown", "dl.start-keep", "dl.start-shutdown", "dl.stop", "dl.keep",
		"dl.shutdown", "dl.after-keep", "dl.after-shutdown":
		// through Island: it stops Download mode, logs it and locks first, as its own buttons do
		if !s.toIsland(map[string]string{"ev": "do", "what": what}) {
			p.send(msg{T: "toast", Msg: "Island isn't running on the PC."})
		}
	default:
		log.Printf("unknown action %q", what)
	}
}

// showPhoneNotif turns a phone notification into a normal desktop one, so Island shows it as a bubble.
func showPhoneNotif(app, title, text string) {
	if title == "" {
		title = app
	}
	spawn("notify-send", "-a", app, "-i", "phone", "-h", "string:desktop-entry:island-link", "--", title, text)
}

// spawn starts a program and forgets it (still reaped, no zombie). Swapped out in tests.
var spawn = func(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		log.Printf("%s: %v", name, err)
		return
	}
	go cmd.Wait()
}

func pcClipboard() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", "text").Output()
	if len(out) > maxLine/2 {
		out = out[:maxLine/2]
	}
	return strings.ToValidUTF8(string(out), "")
}
