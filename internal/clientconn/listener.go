// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clientconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FerretDB/FerretDB/internal/clientconn/connmetrics"
	"github.com/FerretDB/FerretDB/internal/handlers"
	"github.com/FerretDB/FerretDB/internal/util/ctxutil"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
)

// Listener accepts incoming client connections.
type Listener struct {
	*NewListenerOpts
	tcpListener      net.Listener
	sockListener     net.Listener
	tcpListenerReady chan struct{}
}

// NewListenerOpts represents listener configuration.
type NewListenerOpts struct {
	ListenAddr     string
	ListenUnix     string
	ProxyAddr      string
	Mode           Mode
	Metrics        *connmetrics.ListenerMetrics
	Handler        handlers.Interface
	Logger         *zap.Logger
	TestRecordsDir string // if empty, no records are created
}

// NewListener returns a new listener, configured by the NewListenerOpts argument.
func NewListener(opts *NewListenerOpts) *Listener {
	return &Listener{
		NewListenerOpts:  opts,
		tcpListenerReady: make(chan struct{}),
	}
}

// Run runs the listener until ctx is done or some unrecoverable error occurs.
//
// When this method returns, listener and all connections are closed.
func (l *Listener) Run(ctx context.Context) error {
	logger := l.Logger.Named("listener")

	useSock := l.ListenUnix != ""
	useTcp := !useSock || l.ListenAddr != ""

	if useTcp {
		var err error
		if l.tcpListener, err = net.Listen("tcp", l.ListenAddr); err != nil {
			return lazyerrors.Error(err)
		}

		close(l.tcpListenerReady)

		logger.Sugar().Infof("Listening on %s ...", l.Addr())
	}

	if useSock {
		var err error
		if l.sockListener, err = net.Listen("unix", l.ListenUnix); err != nil {
			return lazyerrors.Error(err)
		}

		logger.Sugar().Infof("Listening on %s ...", l.Sock())
	}

	// handle ctx cancellation
	go func() {
		<-ctx.Done()

		if useTcp {
			l.tcpListener.Close()
		}

		if useSock {
			l.sockListener.Close()
		}
	}()

	var wg sync.WaitGroup

	spawnListener := func(ctx context.Context, wg *sync.WaitGroup, l *Listener, listener net.Listener, logger *zap.Logger) {
		wg.Add(1)

		go func() {
			defer wg.Done()
			listenLoop(ctx, wg, l, listener, logger)
		}()
	}

	// handle TCP stream
	if useTcp {
		spawnListener(ctx, &wg, l, l.tcpListener, logger)
	}

	// handle UNIX stream
	if useSock {
		spawnListener(ctx, &wg, l, l.sockListener, logger)
	}

	logger.Info("Waiting for all connections to stop...")
	wg.Wait()

	return ctx.Err()
}

func listenLoop(ctx context.Context, wg *sync.WaitGroup, l *Listener, listener net.Listener, logger *zap.Logger) {
	for {
		netConn, err := listener.Accept()
		if err != nil {
			l.Metrics.Accepts.WithLabelValues("1").Inc()

			if ctx.Err() != nil {
				break
			}

			logger.Warn("Failed to accept connection", zap.Error(err))
			if !errors.Is(err, net.ErrClosed) {
				time.Sleep(time.Second)
			}
			continue
		}

		wg.Add(1)
		l.Metrics.Accepts.WithLabelValues("0").Inc()
		l.Metrics.ConnectedClients.Inc()

		go runConn(ctx, netConn, l, wg, logger)
	}
}

func runConn(ctx context.Context, netConn net.Conn, l *Listener, wg *sync.WaitGroup, logger *zap.Logger) {
	connID := fmt.Sprintf("%s -> %s", netConn.RemoteAddr(), netConn.LocalAddr())

	// give clients a few seconds to disconnect after ctx is canceled
	runCtx, runCancel := ctxutil.WithDelay(ctx.Done(), 3*time.Second)
	defer runCancel()

	defer pprof.SetGoroutineLabels(runCtx)
	runCtx = pprof.WithLabels(runCtx, pprof.Labels("conn", connID))
	pprof.SetGoroutineLabels(runCtx)

	defer func() {
		netConn.Close()
		l.Metrics.ConnectedClients.Dec()
		wg.Done()
	}()

	opts := &newConnOpts{
		netConn:        netConn,
		mode:           l.Mode,
		l:              l.Logger.Named("// " + connID + " "), // derive from the original unnamed logger
		proxyAddr:      l.ProxyAddr,
		handler:        l.Handler,
		connMetrics:    l.Metrics.ConnMetrics,
		testRecordsDir: l.TestRecordsDir,
	}
	conn, e := newConn(opts)
	if e != nil {
		logger.Warn("Failed to create connection", zap.String("conn", connID), zap.Error(e))
		return
	}

	logger.Info("Connection started", zap.String("conn", connID))

	e = conn.run(runCtx)
	if errors.Is(e, io.EOF) {
		logger.Info("Connection stopped", zap.String("conn", connID))
	} else {
		logger.Warn("Connection stopped", zap.String("conn", connID), zap.Error(e))
	}
}

// Addr returns listener's address.
// It can be used to determine an actually used port, if it was zero.
//
// It is a blocking call unless Run was not called.
func (l *Listener) Addr() net.Addr {
	<-l.tcpListenerReady
	return l.tcpListener.Addr()
}

// Sock returns listener's unix domain socket address.
func (l *Listener) Sock() net.Addr {
	// we don't do blocking because we restricted the socket to not has "" path
	// so we always know the address.

	return l.sockListener.Addr()
}

// Describe implements prometheus.Collector.
func (l *Listener) Describe(ch chan<- *prometheus.Desc) {
	l.Metrics.Describe(ch)
}

// Collect implements prometheus.Collector.
func (l *Listener) Collect(ch chan<- prometheus.Metric) {
	l.Metrics.Collect(ch)
}

// check interfaces
var (
	_ prometheus.Collector = (*Listener)(nil)
)
