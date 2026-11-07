package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Waking the phone instead of making it poll:
//
//	pc → phone  {"t":"bye"}    on the main link when this helper stops (shutdown, reboot, log out):
//	                           the phone stops retrying and waits
//	pc → phone  UDP {"t":"knock"} to the phone's last address, port knockPort, when the helper starts
//	                           or the PC wakes from sleep: the phone connects right away
//
// A knock carries nothing secret. It only makes the phone try the normal TLS link, which checks the
// PC's fingerprint, so a fake knock costs at most one connection attempt.
const knockPort = 47801

// knock times after a start or wake: Wi-Fi may need a few seconds to come back first.
var knockAfter = []time.Duration{0, 3 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}

// noteAddr remembers where a phone connected from, so a knock can find it later.
func (s *server) noteAddr(id string, addr net.Addr) {
	ip := ""
	if a, ok := addr.(*net.TCPAddr); ok {
		ip = a.IP.String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.phones[id]
	if !ok || ip == "" || p.Addr == ip {
		return
	}
	p.Addr = ip
	s.phones[id] = p
	if err := savePhones(s.phones); err != nil {
		log.Printf("saving phones: %v", err)
	}
}

// offlineAddrs are the last addresses of the paired phones that aren't connected.
func (s *server) offlineAddrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, p := range s.phones {
		if s.online[id] == nil && p.Addr != "" {
			out = append(out, p.Addr)
		}
	}
	return out
}

// knockAll tells the offline phones that the PC is here, a few times, until they're all back.
func (s *server) knockAll(why string) {
	start := time.Now()
	for _, at := range knockAfter {
		time.Sleep(time.Until(start.Add(at)))
		addrs := s.offlineAddrs()
		if len(addrs) == 0 {
			return
		}
		if at == 0 {
			log.Printf("%s: knocking on %d phone(s)", why, len(addrs))
		}
		for _, a := range addrs {
			knock(a)
		}
	}
}

func knock(ip string) {
	c, err := net.Dial("udp", net.JoinHostPort(ip, strconv.Itoa(knockPort)))
	if err != nil {
		return
	}
	defer c.Close()
	c.Write([]byte(`{"t":"knock"}` + "\n"))
}

// watchWake knocks after the PC wakes up. Go's monotonic clock stops while the PC sleeps and the wall
// clock doesn't, so a gap between them means it slept (no D-Bus needed).
func (s *server) watchWake(every time.Duration) {
	wall, mono := time.Now().Round(0), time.Now()
	for range time.Tick(every) {
		now := time.Now()
		slept := now.Round(0).Sub(wall) - now.Sub(mono)
		wall, mono = now.Round(0), now
		if slept > every {
			// links from before the sleep are dead but would only time out later: drop them so the
			// phones count as offline and get knocked
			for _, p := range s.sessions() {
				p.conn.Close()
			}
			go func() {
				time.Sleep(time.Second)
				s.knockAll("woke up after " + slept.Round(time.Second).String())
			}()
		}
	}
}

// byeOnStop says goodbye to the phones when systemd stops the helper, then exits.
func (s *server) byeOnStop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	s.broadcast(msg{T: "bye"})
	time.Sleep(300 * time.Millisecond) // let the TLS records leave before the process ends
	os.Exit(0)
}
