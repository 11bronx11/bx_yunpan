// Package testfault provides connection faults scoped to integration fixtures.
package testfault

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type TCP struct {
	Address string
	target  string
	listen  net.Listener
	mu      sync.Mutex
	blocked bool
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

func NewTCP(t testing.TB, target string) *TCP {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &TCP{Address: l.Addr().String(), target: target, listen: l, conns: make(map[net.Conn]struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.forward(client)
		}
	}()
	t.Cleanup(func() {
		_ = p.listen.Close()
		p.Cut()
		p.wg.Wait()
	})
	return p
}

func (p *TCP) forward(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.blocked {
		p.mu.Unlock()
		return
	}
	p.conns[client], p.conns[upstream] = struct{}{}, struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, upstream)
		p.mu.Unlock()
	}()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}

// Cut terminates existing sockets and rejects all new connections.
func (p *TCP) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = true
	for c := range p.conns {
		_ = c.Close()
	}
}

func (p *TCP) Restore() {
	p.mu.Lock()
	p.blocked = false
	p.mu.Unlock()
}
