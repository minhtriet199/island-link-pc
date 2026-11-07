package main

import (
	"net"
	"testing"
	"time"
)

// A phone's address is remembered across restarts and an offline phone gets a knock there.
func TestKnockReachesOfflinePhone(t *testing.T) {
	s, _ := testServer(t)
	s.phones["p1"] = newPhone("p1", "Pixel", "k")
	s.noteAddr("p1", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 50000})
	if got := loadPhones()["p1"].Addr; got != "127.0.0.1" {
		t.Fatalf("saved addr = %q", got)
	}

	ln, err := net.ListenPacket("udp", "127.0.0.1:47801")
	if err != nil {
		t.Skip("knock port busy:", err)
	}
	defer ln.Close()
	old := knockAfter
	knockAfter = []time.Duration{0}
	defer func() { knockAfter = old }()

	s.knockAll("test")
	ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := ln.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "{\"t\":\"knock\"}\n" {
		t.Fatalf("knock = %q, %v", buf[:n], err)
	}

	// online phones are left alone
	s.online["p1"] = &session{id: "p1"}
	if a := s.offlineAddrs(); len(a) != 0 {
		t.Fatalf("online phone still knocked: %v", a)
	}
}
