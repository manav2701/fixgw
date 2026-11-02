// Package chaos provides a FIX-aware TCP proxy for failure injection. It sits
// between a FIX initiator and the gateway and can swallow whole client
// messages, creating a real sequence-number gap on the wire, or sever the
// connection outright.
package chaos

import (
	"bytes"
	"io"
	"net"
	"sync"
)

const soh = 0x01

var checksumMarker = []byte{soh, '1', '0', '='}

// Framer splits a TCP byte stream into complete FIX messages. A message ends at
// the SOH that terminates its CheckSum(10) field.
type Framer struct{ buf []byte }

// Push appends data and returns every message completed by it.
func (f *Framer) Push(data []byte) [][]byte {
	f.buf = append(f.buf, data...)
	var out [][]byte
	for {
		i := bytes.Index(f.buf, checksumMarker)
		if i < 0 {
			break
		}
		rest := f.buf[i+len(checksumMarker):]
		k := bytes.IndexByte(rest, soh)
		if k < 0 {
			break
		}
		end := i + len(checksumMarker) + k + 1
		msg := make([]byte, end)
		copy(msg, f.buf[:end])
		out = append(out, msg)
		f.buf = f.buf[end:]
	}
	return out
}

// MsgType returns the MsgType(35) value of a raw FIX message, or "".
func MsgType(msg []byte) string {
	marker := []byte{soh, '3', '5', '='}
	i := bytes.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if k := bytes.IndexByte(rest, soh); k >= 0 {
		return string(rest[:k])
	}
	return ""
}

// Proxy forwards TCP traffic to a target.
type Proxy struct {
	ln     net.Listener
	target string

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	dropType string
	dropLeft int
	dropped  int
	closed   bool
	wg       sync.WaitGroup
}

// Listen starts a proxy on addr (use "127.0.0.1:0" for an ephemeral port).
func Listen(addr, target string) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln, target: target, conns: make(map[net.Conn]struct{})}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

// Addr is the address clients should connect to.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// DropClientMessages swallows the next n client-to-server messages whose
// MsgType is msgType. The sender still believes they were delivered.
func (p *Proxy) DropClientMessages(msgType string, n int) {
	p.mu.Lock()
	p.dropType, p.dropLeft, p.dropped = msgType, n, 0
	p.mu.Unlock()
}

// Dropped reports how many messages have been swallowed since the last
// DropClientMessages call.
func (p *Proxy) Dropped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// KillConnections severs every live connection. New connections are accepted.
func (p *Proxy) KillConnections() {
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
}

// Close stops the proxy and severs all connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	err := p.ln.Close()
	p.KillConnections()
	p.wg.Wait()
	return err
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.serve(client)
	}
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *Proxy) serve(client net.Conn) {
	defer p.wg.Done()
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	if !p.track(client) || !p.track(server) {
		client.Close()
		server.Close()
		return
	}
	defer p.untrack(client)
	defer p.untrack(server)

	done := make(chan struct{}, 2)
	go func() {
		p.pumpClientToServer(client, server)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, server)
		done <- struct{}{}
	}()
	<-done
}

func (p *Proxy) shouldDrop(msg []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropLeft > 0 && MsgType(msg) == p.dropType {
		p.dropLeft--
		p.dropped++
		return true
	}
	return false
}

func (p *Proxy) pumpClientToServer(client, server net.Conn) {
	var fr Framer
	buf := make([]byte, 4096)
	for {
		n, err := client.Read(buf)
		for _, msg := range fr.Push(buf[:n]) {
			if p.shouldDrop(msg) {
				continue
			}
			if _, werr := server.Write(msg); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
