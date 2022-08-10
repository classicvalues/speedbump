// Package lib allows for using speedbump as a library.
package lib

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// Speedbump is a proxy instance returned by NewSpeedbump
type Speedbump struct {
	bufferSize        int
	queueSize         int
	srcAddr, destAddr net.TCPAddr
	listener          *net.TCPListener
	latencyGen        LatencyGenerator
	nextConnId        int
	// active keeps track of proxy connections that are running
	active sync.WaitGroup
	// ctx is used for notifying proxy connections once Stop() is invoked
	ctx       context.Context
	ctxCancel context.CancelFunc
	log       hclog.Logger
}

// SpeedbumpCfg contains Spedbump instance configuration
type SpeedbumpCfg struct {
	// Port specifies the local port number to listen on
	Port int
	// DestAddr specifies the proxy desination address in host:port format
	DestAddr string
	// BufferSize specifies the number of bytes in a buffer used for TCP reads
	BufferSize int
	// The size of the delay queue containing read buffers (defaults to 1024)
	QueueSize int
	// LatencyCfg specifies parameters of the desired latency summands
	Latency *LatencyCfg
	// LogLevel can be one of: DEBUG, TRACE, INFO, WARN, ERROR
	LogLevel string
}

// NewSpeedbump creates a Speedbump instance based on a provided config
func NewSpeedbump(cfg *SpeedbumpCfg) (*Speedbump, error) {
	localTCPAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("Error resolving local address: %s", err)
	}
	destTCPAddr, err := net.ResolveTCPAddr("tcp", cfg.DestAddr)
	if err != nil {
		return nil, fmt.Errorf("Error resolving destination address: %s", err)
	}
	l := hclog.New(&hclog.LoggerOptions{
		Level: hclog.LevelFromString(cfg.LogLevel),
	})
	queueSize := cfg.QueueSize
	// setting a default queueSize in order to maintain compatibility
	// with speedbump @v0.1.0 used as a dependency in other Go programs
	if queueSize == 0 {
		queueSize = 1024
	}
	s := &Speedbump{
		bufferSize: int(cfg.BufferSize),
		queueSize:  queueSize,
		srcAddr:    *localTCPAddr,
		destAddr:   *destTCPAddr,
		latencyGen: newSimpleLatencyGenerator(time.Now(), cfg.Latency),
		log:        l,
	}
	return s, nil
}

func (s *Speedbump) startAcceptLoop() {
	for {
		conn, err := s.listener.AcceptTCP()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed") {
				// the listener was closed, which means that Stop() was called
				return
			} else {
				s.log.Warn("Accepting incoming TCP conn failed", "err", err)
				continue
			}
		}
		l := s.log.With("connection", s.nextConnId)
		p, err := newProxyConnection(
			s.ctx,
			conn,
			&s.srcAddr,
			&s.destAddr,
			s.bufferSize,
			s.queueSize,
			s.latencyGen,
			l,
		)
		if err != nil {
			s.log.Warn("Creating new proxy conn failed", "err", err)
			conn.Close()
			continue
		}
		s.nextConnId++
		s.active.Add(1)
		go s.startProxyConnection(p)
	}
}

func (s *Speedbump) startProxyConnection(p *connection) {
	defer s.active.Done()
	// start will block until a proxy connection is closed
	p.start()
}

// Start launches a Speedbump instance. This operation will unblock either
// as soon as the proxy starts listening or when a startup error occurrs.
func (s *Speedbump) Start() error {
	listener, err := net.ListenTCP("tcp", &s.srcAddr)
	if err != nil {
		return fmt.Errorf("Error starting TCP listener: %s", err)
	}
	s.listener = listener

	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.ctxCancel = cancel

	s.log.Info("Started speedbump", "port", s.srcAddr.Port, "dest", s.destAddr.String())

	go s.startAcceptLoop()
	return nil
}

// Stop closes the Speedbump instance's TCP listener and notifies all existing
// proxy connections that Speedbump is shutting down. It waits for individual
// proxy connections to close before returning.
func (s *Speedbump) Stop() {
	s.log.Info("Stopping speedbump")
	// close TCP listener so that startAcceptLoop returns
	s.listener.Close()
	// notify all proxy connections
	s.ctxCancel()
	s.log.Debug("Waiting for active connections to be closed")
	s.active.Wait()
	s.log.Info("Speedbump stopped")
}
