// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package lsprpc implements a jsonrpc2.StreamServer that may be used to
// serve the LSP on a jsonrpc2 channel.
package lsprpc

import (
	"context"
	"encoding/json"
	stdlog "log"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/anz-bank/sysl/pkg/lspimpl"
	"github.com/anz-bank/sysl/pkg/lspimpl/lspframework/jsonrpc2"
	"github.com/anz-bank/sysl/pkg/lspimpl/lspframework/lsp/debug"
	"github.com/anz-bank/sysl/pkg/lspimpl/lspframework/lsp/protocol"
	"github.com/anz-bank/sysl/pkg/lspimpl/lspframework/telemetry/log"
)

// AutoNetwork is the pseudo network type used to signal that gopls should use
// automatic discovery to resolve a remote address.
const AutoNetwork = "auto"

// The StreamServer type is a jsonrpc2.StreamServer that handles incoming
// streams as a new LSP session, using a shared cache.
type StreamServer struct {
	withTelemetry bool

	// MODIFIED: SYSL_LSP
	//cache         *cache.Cache

	// serverForTest may be set to a test fake for testing.
	serverForTest protocol.Server
}

var clientIndex, serverIndex int64

// NewStreamServer creates a StreamServer using the shared cache. If
// withTelemetry is true, each session is instrumented with telemetry that
// records RPC statistics.
// MODIFIED: SYSL_LSP
//func NewStreamServer(cache *cache.Cache, withTelemetry bool) *StreamServer {
func NewStreamServer(withTelemetry bool) *StreamServer {
	s := &StreamServer{
		withTelemetry: withTelemetry,
		// MODIFIED: SYSL_LSP
		//cache:         cache,
	}
	return s
}

// debugInstance is the common functionality shared between client and server
// gopls instances.
type debugInstance struct {
	id           string
	debugAddress string
	logfile      string
	goplsPath    string
}

func (d debugInstance) ID() string {
	return d.id
}

func (d debugInstance) DebugAddress() string {
	return d.debugAddress
}

func (d debugInstance) Logfile() string {
	return d.logfile
}

func (d debugInstance) GoplsPath() string {
	return d.goplsPath
}

// A debugServer is held by the client to identity the remove server to which
// it is connected.
type debugServer struct {
	debugInstance
	// clientID is the id of this client on the server.
	clientID string
}

func (s debugServer) ClientID() string {
	return s.clientID
}

// A debugClient is held by the server to identify an incoming client
// connection.
type debugClient struct {
	debugInstance
	// session is the session serving this client.
	// MODIFIED: SYSL_LSP
	//session *cache.Session
	// serverID is this id of this server on the client.
	serverID string
}

// MODIFIED: SYSL_LSP
/*
func (c debugClient) Session() debug.Session {
	return cache.DebugSession{Session: c.session}
}
*/

func (c debugClient) ServerID() string {
	return c.serverID
}

// ServeStream implements the jsonrpc2.StreamServer interface, by handling
// incoming streams using a new lsp server.
func (s *StreamServer) ServeStream(ctx context.Context, stream jsonrpc2.Stream) error {
	index := atomic.AddInt64(&clientIndex, 1)

	conn := jsonrpc2.NewConn(stream)
	client := protocol.ClientDispatcher(conn)
	dc := &debugClient{
		debugInstance: debugInstance{
			id: strconv.FormatInt(index, 10),
		},
		// MODIFIED: SYSL_LSP
		//session: session,
	}
	if di := debug.GetInstance(ctx); di != nil {
		di.State.AddClient(dc)
		defer di.State.DropClient(dc)
	}
	server := s.serverForTest
	if server == nil {
		// MODIFIED: SYSL_LSP
		//server = lsp.NewServer(session, client)
		server = lspimpl.NewServer(client)
	}
	// Clients may or may not send a shutdown message. Make sure the server is
	// shut down.
	// TODO(rFindley): this shutdown should perhaps be on a disconnected context.
	defer server.Shutdown(ctx)
	conn.AddHandler(protocol.ServerHandler(server))
	conn.AddHandler(protocol.Canceller{})
	if s.withTelemetry {
		conn.AddHandler(telemetryHandler{})
	}
	executable, err := os.Executable()
	if err != nil {
		stdlog.Printf("error getting gopls path: %v", err)
		executable = ""
	}
	conn.AddHandler(&handshaker{
		client:    dc,
		goplsPath: executable,
	})
	return conn.Run(protocol.WithClient(ctx, client))
}

// MODIFIED: SYSL_LSP
/*
// A Forwarder is a jsonrpc2.StreamServer that handles an LSP stream by
// forwarding it to a remote. This is used when the gopls process started by
// the editor is in the `-remote` mode, which means it finds and connects to a
// separate gopls daemon. In these cases, we still want the forwarder gopls to
// be instrumented with telemetry, and want to be able to in some cases hijack
// the jsonrpc2 connection with the daemon.
type Forwarder struct {
	network, addr string

	// Configuration. Right now, not all of this may be customizable, but in the
	// future it probably will be.
	withTelemetry bool
	dialTimeout   time.Duration
	retries       int
	goplsPath     string
}

// NewForwarder creates a new Forwarder, ready to forward connections to the
// remote server specified by network and addr.
func NewForwarder(network, addr string, withTelemetry bool) *Forwarder {
	gp, err := os.Executable()
	if err != nil {
		stdlog.Printf("error getting gopls path for forwarder: %v", err)
		gp = ""
	}

	return &Forwarder{
		network:       network,
		addr:          addr,
		withTelemetry: withTelemetry,
		dialTimeout:   1 * time.Second,
		retries:       5,
		goplsPath:     gp,
	}
}

// ServeStream dials the forwarder remote and binds the remote to serve the LSP
// on the incoming stream.
func (f *Forwarder) ServeStream(ctx context.Context, stream jsonrpc2.Stream) error {
	clientConn := jsonrpc2.NewConn(stream)
	client := protocol.ClientDispatcher(clientConn)

	netConn, err := f.connectToRemote(ctx)
	if err != nil {
		return fmt.Errorf("forwarder: connecting to remote: %v", err)
	}
	serverConn := jsonrpc2.NewConn(jsonrpc2.NewHeaderStream(netConn, netConn))
	server := protocol.ServerDispatcher(serverConn)

	// Forward between connections.
	serverConn.AddHandler(protocol.ClientHandler(client))
	serverConn.AddHandler(protocol.Canceller{})
	clientConn.AddHandler(protocol.ServerHandler(server))
	clientConn.AddHandler(protocol.Canceller{})
	clientConn.AddHandler(forwarderHandler{})
	if f.withTelemetry {
		clientConn.AddHandler(telemetryHandler{})
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return serverConn.Run(ctx)
	})
	// Don't run the clientConn yet, so that we can complete the handshake before
	// processing any client messages.

	// Do a handshake with the server instance to exchange debug information.
	index := atomic.AddInt64(&serverIndex, 1)
	serverID := strconv.FormatInt(index, 10)
	di := debug.GetInstance(ctx)
	var (
		hreq = handshakeRequest{
			ServerID:  serverID,
			GoplsPath: f.goplsPath,
		}
		hresp handshakeResponse
	)
	if di != nil {
		hreq.Logfile = di.Logfile
		hreq.DebugAddr = di.ListenedDebugAddress
	}
	if err := serverConn.Call(ctx, handshakeMethod, hreq, &hresp); err != nil {
		log.Error(ctx, "forwarder: gopls handshake failed", err)
	}
	if hresp.GoplsPath != f.goplsPath {
		log.Error(ctx, "", fmt.Errorf("forwarder: gopls path mismatch: forwarder is %q, remote is %q", f.goplsPath, hresp.GoplsPath))
	}
	if di != nil {
		di.State.AddServer(debugServer{
			debugInstance: debugInstance{
				id:           serverID,
				logfile:      hresp.Logfile,
				debugAddress: hresp.DebugAddr,
				goplsPath:    hresp.GoplsPath,
			},
			clientID: hresp.ClientID,
		})
	}
	g.Go(func() error {
		return clientConn.Run(ctx)
	})

	return g.Wait()
}

func (f *Forwarder) connectToRemote(ctx context.Context) (net.Conn, error) {
	var (
		netConn          net.Conn
		err              error
		network, address = f.network, f.addr
	)
	if f.network == AutoNetwork {
		// f.network is overloaded to support a concept of 'automatic' addresses,
		// which signals that the gopls remote address should be automatically
		// derived.
		// So we need to resolve a real network and address here.
		network, address = autoNetworkAddress(f.goplsPath, f.addr)
	}
	// Attempt to verify that we own the remote. This is imperfect, but if we can
	// determine that the remote is owned by a different user, we should fail.
	ok, err := verifyRemoteOwnership(network, address)
	if err != nil {
		// If the ownership check itself failed, we fail open but log an error to
		// the user.
		log.Error(ctx, "unable to check daemon socket owner, failing open: %v", err)
	} else if !ok {
		// We succesfully checked that the socket is not owned by us, we fail
		// closed.
		return nil, fmt.Errorf("socket %q is owned by a different user", address)
	}
	// Try dialing our remote once, in case it is already running.
	netConn, err = net.DialTimeout(network, address, f.dialTimeout)
	if err == nil {
		return netConn, nil
	}
	// If our remote is on the 'auto' network, start it if it doesn't exist.
	if f.network == AutoNetwork {
		if f.goplsPath == "" {
			return nil, fmt.Errorf("cannot auto-start remote: gopls path is unknown")
		}
		if network == "unix" {
			// Sometimes the socketfile isn't properly cleaned up when gopls shuts
			// down. Since we have already tried and failed to dial this address, it
			// should *usually* be safe to remove the socket before binding to the
			// address.
			// TODO(rfindley): there is probably a race here if multiple gopls
			// instances are simultaneously starting up.
			if _, err := os.Stat(address); err == nil {
				if err := os.Remove(address); err != nil {
					return nil, fmt.Errorf("removing remote socket file: %v", err)
				}
			}
		}
		args := []string{"serve",
			"-listen", fmt.Sprintf(`%s;%s`, network, address),
			"-listen.timeout", "1m",
			"-debug", "localhost:0",
			"-logfile", "auto",
		}
		if err := startRemote(f.goplsPath, args...); err != nil {
			return nil, fmt.Errorf("startRemote(%q, %v): %v", f.goplsPath, args, err)
		}
	}

	// It can take some time for the newly started server to bind to our address,
	// so we retry for a bit.
	for retry := 0; retry < f.retries; retry++ {
		startDial := time.Now()
		netConn, err = net.DialTimeout(network, address, f.dialTimeout)
		if err == nil {
			return netConn, nil
		}
		log.Print(ctx, fmt.Sprintf("failed attempt #%d to connect to remote: %v\n", retry+2, err))
		// In case our failure was a fast-failure, ensure we wait at least
		// f.dialTimeout before trying again.
		if retry != f.retries-1 {
			time.Sleep(f.dialTimeout - time.Since(startDial))
		}
	}
	return nil, fmt.Errorf("dialing remote: %v", err)
}

// ForwarderExitFunc is used to exit the forwarder process. It is mutable for
// testing purposes.
var ForwarderExitFunc = os.Exit

// OverrideExitFuncsForTest can be used from test code to prevent the test
// process from exiting on server shutdown. The returned func reverts the exit
// funcs to their previous state.
func OverrideExitFuncsForTest() func() {
	// Override functions that would shut down the test process
	cleanup := func(lspExit, forwarderExit func(code int)) func() {
		return func() {
			// MODIFIED: SYSL_LSP
			lspimpl.ServerExitFunc = lspExit
			ForwarderExitFunc = forwarderExit
		}
		// MODIFIED: SYSL_LSP
	}(lspimpl.ServerExitFunc, ForwarderExitFunc)
	// It is an error for a test to shutdown a server process.
	// MODIFIED: SYSL_LSP
	lspimpl.ServerExitFunc = func(code int) {
		panic(fmt.Sprintf("LSP server exited with code %d", code))
	}
	// We don't want our forwarders to exit, but it's OK if they would have.
	ForwarderExitFunc = func(code int) {}
	return cleanup
}

// forwarderHandler intercepts 'exit' messages to prevent the shared gopls
// instance from exiting. In the future it may also intercept 'shutdown' to
// provide more graceful shutdown of the client connection.
type forwarderHandler struct {
	jsonrpc2.EmptyHandler
}

func (forwarderHandler) Deliver(ctx context.Context, r *jsonrpc2.Request, delivered bool) bool {
	// TODO(golang.org/issues/34111): we should more gracefully disconnect here,
	// once that process exists.
	if r.Method == "exit" {
		ForwarderExitFunc(0)
		// Still return true here to prevent the message from being delivered: in
		// tests, ForwarderExitFunc may be overridden to something that doesn't
		// exit the process.
		return true
	}
	return false
}
*/

type handshaker struct {
	jsonrpc2.EmptyHandler
	client    *debugClient
	goplsPath string
}

type handshakeRequest struct {
	ServerID  string `json:"serverID"`
	Logfile   string `json:"logfile"`
	DebugAddr string `json:"debugAddr"`
	GoplsPath string `json:"goplsPath"`
}

type handshakeResponse struct {
	ClientID  string `json:"clientID"`
	SessionID string `json:"sessionID"`
	Logfile   string `json:"logfile"`
	DebugAddr string `json:"debugAddr"`
	GoplsPath string `json:"goplsPath"`
}

const handshakeMethod = "gopls/handshake"

func (h *handshaker) Deliver(ctx context.Context, r *jsonrpc2.Request, delivered bool) bool {
	if r.Method == handshakeMethod {
		var req handshakeRequest
		if err := json.Unmarshal(*r.Params, &req); err != nil {
			sendError(ctx, r, err)
			return true
		}
		h.client.debugAddress = req.DebugAddr
		h.client.logfile = req.Logfile
		h.client.serverID = req.ServerID
		h.client.goplsPath = req.GoplsPath
		resp := handshakeResponse{
			ClientID: h.client.id,
			// MODIFIED: SYSL_LSP
			//SessionID: cache.DebugSession{Session: h.client.session}.ID(),
			SessionID: "0",
			GoplsPath: h.goplsPath,
		}
		if di := debug.GetInstance(ctx); di != nil {
			resp.Logfile = di.Logfile
			resp.DebugAddr = di.ListenedDebugAddress
		}

		if err := r.Reply(ctx, resp, nil); err != nil {
			log.Error(ctx, "replying to handshake", err)
		}
		return true
	}
	return false
}

func sendError(ctx context.Context, req *jsonrpc2.Request, err error) {
	if _, ok := err.(*jsonrpc2.Error); !ok {
		err = jsonrpc2.NewErrorf(jsonrpc2.CodeParseError, "%v", err)
	}
	if err := req.Reply(ctx, nil, err); err != nil {
		log.Error(ctx, "", err)
	}
}
