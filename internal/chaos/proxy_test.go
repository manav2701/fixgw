package chaos

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func fixMsg(msgType, extra string) []byte {
	return []byte("8=FIX.4.4\x019=10\x0135=" + msgType + "\x01" + extra + "10=123\x01")
}

func TestFramerSplitsStream(t *testing.T) {
	a := fixMsg("D", "11=a\x01")
	b := fixMsg("0", "")
	stream := append(append([]byte{}, a...), b...)

	var fr Framer
	var got [][]byte
	// Feed one byte at a time to exercise every split point.
	for i := range stream {
		got = append(got, fr.Push(stream[i:i+1])...)
	}
	if len(got) != 2 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Fatalf("got %q", got)
	}
}

func TestMsgType(t *testing.T) {
	if got := MsgType(fixMsg("D", "")); got != "D" {
		t.Errorf("MsgType = %q", got)
	}
	if got := MsgType([]byte("garbage")); got != "" {
		t.Errorf("MsgType(garbage) = %q", got)
	}
}

func TestProxyDropsAndKills(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		received <- data
	}()

	p, err := Listen("127.0.0.1:0", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	p.DropClientMessages("D", 2)
	keep1, drop1, drop2, keep2, keep3 := fixMsg("A", ""), fixMsg("D", "11=1\x01"), fixMsg("D", "11=2\x01"), fixMsg("D", "11=3\x01"), fixMsg("0", "")
	for _, m := range [][]byte{keep1, drop1, drop2, keep2, keep3} {
		if _, err := c.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	// Wait for the proxy to consume them all, then sever.
	deadline := time.Now().Add(2 * time.Second)
	for p.Dropped() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	p.KillConnections()

	select {
	case data := <-received:
		want := append(append(append([]byte{}, keep1...), keep2...), keep3...)
		if !bytes.Equal(data, want) {
			t.Errorf("server saw\n%q\nwant\n%q", data, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never saw EOF after KillConnections")
	}
	if p.Dropped() != 2 {
		t.Errorf("Dropped = %d", p.Dropped())
	}
}
