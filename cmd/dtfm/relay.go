// Copyright 2026 Ivan Inozemtsev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iinozemtsev-lab/dtfm/pkg/e2ee"
	"github.com/iinozemtsev-lab/dtfm/pkg/protocol"
)

const (
	writeWait                  = 10 * time.Second
	pongWait                   = 90 * time.Second
	pingPeriod                 = (pongWait * 9) / 10
	maxReadBufferCap           = protocol.MaxMessageDataBytes + 128*1024
	maxWriteBufferCap          = protocol.MaxResponseEnvelopeBytes
	maxOperatorWSReadBytes     = 6 * 1024 * 1024
	defaultHandshakeTimeout    = 10 * time.Second
	defaultReconnectMinBackoff = 1 * time.Second
	defaultReconnectMaxBackoff = 5 * time.Second
	wsCloseCodeForbidden       = 4003
	wsCloseCodeBadRequest      = 4000
)

type relayAddr struct {
	network string
	str     string
}

func (a relayAddr) Network() string { return a.network }
func (a relayAddr) String() string  { return a.str }

// RelayOptions configures optional RelayListener behaviors (primarily for testing).
type RelayOptions struct {
	MinBackoff time.Duration
	MaxBackoff time.Duration
	LogWriter  io.Writer
	Dialer     *websocket.Dialer
}

// RelayListener implements net.Listener over a persistent WebSocket connection to the relay server.
type RelayListener struct {
	wsConn        *websocket.Conn
	connGen       uint64
	wsWriteMu     sync.Mutex
	sessionID     string
	secretKey     []byte
	handshakeKey  []byte
	relayURL      string
	dialURL       string
	dialHeaders   http.Header
	dialer        *websocket.Dialer
	minBackoff    time.Duration
	maxBackoff    time.Duration
	logWriter     io.Writer
	logMu         sync.Mutex
	acceptCh      chan net.Conn
	doneCh        chan struct{}
	pumpDone      chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	mu            sync.Mutex
	activeConn    *RelayConn
	disconnectErr error
	closed        atomic.Bool
}

// DialRelayListener establishes an authenticated operator connection to the relay server and returns a net.Listener.
func DialRelayListener(relayWsURL, sessionID string, secretKey []byte) (*RelayListener, error) {
	return DialRelayListenerWithOptions(relayWsURL, sessionID, secretKey, nil)
}

// DialRelayListenerWithOptions establishes an authenticated operator connection to the relay server with custom options.
func DialRelayListenerWithOptions(relayWsURL, sessionID string, secretKey []byte, opts *RelayOptions) (*RelayListener, error) {
	if err := e2ee.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID: %w", err)
	}
	if len(secretKey) != e2ee.KeySize {
		return nil, fmt.Errorf("invalid secret key length: expected %d bytes (256 bits), got %d", e2ee.KeySize, len(secretKey))
	}

	opAuthHex, _, agentVerifierHex, err := e2ee.DeriveAuthTokens(secretKey, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to derive relay auth tokens: %w", err)
	}

	handshakeKey, err := e2ee.HandshakeKey(secretKey, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to derive handshake key: %w", err)
	}

	u, err := url.Parse(relayWsURL)
	if err != nil {
		return nil, fmt.Errorf("invalid relay websocket url: %w", err)
	}

	q := u.Query()
	q.Set("session", sessionID)
	q.Set("role", e2ee.RoleOperator)
	u.RawQuery = q.Encode()
	dialURL := u.String()

	headers := make(http.Header)
	headers.Set("X-DTFM-Auth", opAuthHex)
	headers.Set("X-DTFM-Agent-Verifier", agentVerifierHex)

	var dialer websocket.Dialer
	if opts != nil && opts.Dialer != nil {
		dialer = *opts.Dialer
		if dialer.HandshakeTimeout <= 0 {
			dialer.HandshakeTimeout = defaultHandshakeTimeout
		}
	} else {
		dialer = *websocket.DefaultDialer
		dialer.HandshakeTimeout = defaultHandshakeTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())

	wsConn, resp, err := dialer.DialContext(ctx, dialURL, headers.Clone())
	if err != nil {
		cancel()
		if resp != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			switch resp.StatusCode {
			case http.StatusForbidden:
				return nil, formatRelayForbiddenError()
			case http.StatusServiceUnavailable:
				return nil, fmt.Errorf("relay server is at maximum session capacity (HTTP 503)")
			default:
				return nil, fmt.Errorf("relay connection failed (HTTP %d): %w", resp.StatusCode, err)
			}
		}
		return nil, fmt.Errorf("failed to connect to relay server at %s: %w", relayWsURL, err)
	}

	wsConn.SetReadLimit(maxOperatorWSReadBytes)

	minBackoff := defaultReconnectMinBackoff
	maxBackoff := defaultReconnectMaxBackoff
	var logWriter io.Writer = os.Stderr
	if opts != nil {
		if opts.MinBackoff > 0 {
			minBackoff = opts.MinBackoff
		}
		if opts.MaxBackoff > 0 {
			maxBackoff = opts.MaxBackoff
		}
		if opts.LogWriter != nil {
			logWriter = opts.LogWriter
		}
	}

	rl := &RelayListener{
		wsConn:       wsConn,
		connGen:      1,
		sessionID:    sessionID,
		secretKey:    secretKey,
		handshakeKey: handshakeKey,
		relayURL:     relayWsURL,
		dialURL:      dialURL,
		dialHeaders:  headers,
		dialer:       &dialer,
		minBackoff:   minBackoff,
		maxBackoff:   maxBackoff,
		logWriter:    logWriter,
		acceptCh:     make(chan net.Conn, 16),
		doneCh:       make(chan struct{}),
		pumpDone:     make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}

	// Read pump & auto-reconnect loop (manages per-connection pingLoop)
	go rl.readPump(wsConn, 1)

	return rl, nil
}

func (l *RelayListener) logf(format string, args ...any) {
	if l.logWriter == nil {
		return
	}
	l.logMu.Lock()
	defer l.logMu.Unlock()
	if l.closed.Load() {
		return
	}
	_, _ = fmt.Fprintf(l.logWriter, format, args...)
}

func isPermanentHTTPStatus(code int) bool {
	switch code {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusGone:
		return true
	default:
		return false
	}
}

func formatRelayForbiddenError() error {
	return fmt.Errorf("relay authentication failed: invalid token or session mismatch (HTTP 403 Forbidden)")
}

func formatPermanentRelayError(code int, err error) error {
	if code == http.StatusForbidden {
		return formatRelayForbiddenError()
	}
	return fmt.Errorf("relay connection rejected permanently (HTTP %d): %w", code, err)
}

func isPermanentWSCloseError(err error) (error, bool) {
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return nil, false
	}
	switch closeErr.Code {
	case wsCloseCodeForbidden:
		return formatRelayForbiddenError(), true
	case wsCloseCodeBadRequest:
		return fmt.Errorf("relay connection rejected permanently (close %d: %s)", closeErr.Code, closeErr.Text), true
	default:
		return nil, false
	}
}

func jitteredDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// +/- 20% jitter: multiplier in [0.8, 1.2)
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * jitter)
}

func (l *RelayListener) writeWS(ws *websocket.Conn, gen uint64, msgType int, data []byte, deadline time.Time) error {
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}

	l.wsWriteMu.Lock()
	defer l.wsWriteMu.Unlock()

	if l.closed.Load() || l.wsConn == nil || l.wsConn != ws || l.connGen != gen {
		return net.ErrClosed
	}

	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}

	effectiveDeadline := deadline
	if effectiveDeadline.IsZero() {
		effectiveDeadline = time.Now().Add(writeWait)
	}
	_ = ws.SetWriteDeadline(effectiveDeadline)

	if err := ws.WriteMessage(msgType, data); err != nil {
		_ = ws.Close()
		if l.wsConn == ws {
			l.wsConn = nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return os.ErrDeadlineExceeded
		}
		return err
	}
	return nil
}

func (l *RelayListener) pingLoop(ws *websocket.Conn, gen uint64, connDone <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-l.doneCh:
			return
		case <-connDone:
			return
		case <-ticker.C:
			if err := l.writeWS(ws, gen, websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

func (l *RelayListener) sendKickAgentForConn(ws *websocket.Conn, gen uint64, connID uint64) {
	hdr := make([]byte, 9)
	hdr[0] = e2ee.FrameKickAgent
	binary.BigEndian.PutUint64(hdr[1:9], connID)
	_ = l.writeWS(ws, gen, websocket.BinaryMessage, hdr, time.Now().Add(writeWait))
}

func (l *RelayListener) readPump(ws *websocket.Conn, gen uint64) {
	defer close(l.pumpDone)

	for {
		readErr := l.runConnection(ws, gen)

		if l.closed.Load() {
			return
		}

		if permErr, ok := isPermanentWSCloseError(readErr); ok {
			l.mu.Lock()
			if l.disconnectErr == nil {
				l.disconnectErr = permErr
			}
			l.mu.Unlock()
			l.closeInternal()
			return
		}

		l.logf("\n⚠️  Relay connection lost; reconnecting...\n")

		nextWS, err := l.reconnectLoop()
		if err != nil {
			if !l.closed.Load() {
				l.mu.Lock()
				if l.disconnectErr == nil {
					l.disconnectErr = err
				}
				l.mu.Unlock()
				l.closeInternal()
			}
			return
		}

		gen++
		l.wsWriteMu.Lock()
		if l.closed.Load() {
			l.wsWriteMu.Unlock()
			_ = nextWS.Close()
			return
		}
		l.wsConn = nextWS
		l.connGen = gen
		l.wsWriteMu.Unlock()

		ws = nextWS
		l.logf("\n🟢 Relay connection restored (session active).\n")
	}
}

func (l *RelayListener) reconnectLoop() (*websocket.Conn, error) {
	backoff := l.minBackoff
	if backoff <= 0 {
		backoff = defaultReconnectMinBackoff
	}
	maxBackoff := l.maxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultReconnectMaxBackoff
	}
	if maxBackoff < backoff {
		maxBackoff = backoff
	}

	for {
		delay := jitteredDuration(backoff)
		timer := time.NewTimer(delay)
		select {
		case <-l.doneCh:
			timer.Stop()
			return nil, net.ErrClosed
		case <-timer.C:
		}

		if l.closed.Load() {
			return nil, net.ErrClosed
		}

		ws, resp, err := l.dialer.DialContext(l.ctx, l.dialURL, l.dialHeaders.Clone())
		if err == nil {
			ws.SetReadLimit(maxOperatorWSReadBytes)
			return ws, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}

		if l.closed.Load() || l.ctx.Err() != nil {
			return nil, net.ErrClosed
		}

		if resp != nil && isPermanentHTTPStatus(resp.StatusCode) {
			return nil, formatPermanentRelayError(resp.StatusCode, err)
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (l *RelayListener) runConnection(ws *websocket.Conn, gen uint64) error {
	connDone := make(chan struct{})
	go l.pingLoop(ws, gen, connDone)

	defer func() {
		close(connDone)
		_ = ws.Close()

		l.wsWriteMu.Lock()
		if l.wsConn == ws {
			l.wsConn = nil
		}
		l.wsWriteMu.Unlock()

		l.mu.Lock()
		prev := l.activeConn
		l.activeConn = nil
		l.mu.Unlock()
		if prev != nil {
			prev.signalEOF()
		}
	}()

	_ = ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_ = ws.SetReadDeadline(time.Now().Add(pongWait))
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			return err
		}

		if msgType != websocket.BinaryMessage || len(msg) < 9 {
			continue
		}

		ctrlType := msg[0]
		connID := binary.BigEndian.Uint64(msg[1:9])

		switch ctrlType {
		case e2ee.FrameAgentConnected:
			l.mu.Lock()
			prev := l.activeConn
			conn := newRelayConnForGen(l, ws, gen, connID)
			l.activeConn = conn
			l.mu.Unlock()
			if prev != nil {
				prev.signalEOF()
			}

			// Send operator handshake immediately to agent
			opNonce, opHandshakePacket, err := e2ee.BuildOperatorHandshake(l.handshakeKey, l.sessionID)
			if err != nil {
				conn.kickAndClose()
				continue
			}
			conn.opNonce = opNonce

			outFrame := make([]byte, 9+len(opHandshakePacket))
			outFrame[0] = e2ee.FrameData
			binary.BigEndian.PutUint64(outFrame[1:9], connID)
			copy(outFrame[9:], opHandshakePacket)

			if writeErr := l.writeWS(ws, gen, websocket.BinaryMessage, outFrame, time.Now().Add(writeWait)); writeErr != nil {
				conn.kickAndClose()
				return writeErr
			}

			if l.closed.Load() {
				conn.kickAndClose()
				return nil
			}

			select {
			case <-l.doneCh:
				return nil
			case l.acceptCh <- conn:
			default:
				conn.kickAndClose()
			}

		case e2ee.FrameAgentDisconnected:
			l.mu.Lock()
			var prev *RelayConn
			if l.activeConn != nil && l.activeConn.connID == connID && l.activeConn.connGen == gen {
				prev = l.activeConn
				l.activeConn = nil
			}
			l.mu.Unlock()
			if prev != nil {
				prev.signalEOF()
			}

		case e2ee.FrameData:
			payload := msg[9:]
			l.mu.Lock()
			conn := l.activeConn
			l.mu.Unlock()

			if conn != nil && conn.connID == connID && conn.connGen == gen {
				conn.feed(payload)
			}
		}
	}
}

func (l *RelayListener) acceptClosedErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disconnectErr != nil {
		return l.disconnectErr
	}
	return net.ErrClosed
}

func (l *RelayListener) Accept() (net.Conn, error) {
	for {
		select {
		case <-l.doneCh:
			return nil, l.acceptClosedErr()
		default:
		}

		select {
		case <-l.doneCh:
			return nil, l.acceptClosedErr()
		case conn := <-l.acceptCh:
			if l.closed.Load() {
				_ = conn.Close()
				return nil, l.acceptClosedErr()
			}
			if rc, ok := conn.(*RelayConn); ok && rc.IsClosedOrEOF() {
				continue
			}
			return conn, nil
		}
	}
}

func (l *RelayListener) closeInternal() {
	l.closeOnce.Do(func() {
		l.logMu.Lock()
		l.closed.Store(true)
		l.logMu.Unlock()

		close(l.doneCh)
		if l.cancel != nil {
			l.cancel()
		}

		l.wsWriteMu.Lock()
		if l.wsConn != nil {
			_ = l.wsConn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "listener closed"),
				time.Now().Add(time.Second))
			_ = l.wsConn.Close()
			l.wsConn = nil
		}
		l.wsWriteMu.Unlock()

		l.mu.Lock()
		prev := l.activeConn
		l.activeConn = nil
		l.mu.Unlock()
		if prev != nil {
			prev.signalEOF()
		}
	})
}

func (l *RelayListener) Close() error {
	l.closeInternal()
	if l.pumpDone != nil {
		<-l.pumpDone
	}
	return nil
}

func (l *RelayListener) Addr() net.Addr {
	return relayAddr{network: "relay", str: l.sessionID}
}

// RelayConn represents an active agent session bridged over the relay with E2EE.
type RelayConn struct {
	listener      *RelayListener
	wsConn        *websocket.Conn
	connGen       uint64
	connID        uint64
	opNonce       []byte
	connCipher    atomic.Pointer[e2ee.ConnCipher]
	readBuf       bytes.Buffer
	readMu        sync.Mutex
	readCond      *sync.Cond
	readDeadline  time.Time
	writeMu       sync.Mutex
	writeBuf      bytes.Buffer
	writeDeadline time.Time
	closed        atomic.Bool
	remoteEOF     atomic.Bool
}

func newRelayConnForGen(l *RelayListener, ws *websocket.Conn, gen uint64, connID uint64) *RelayConn {
	rc := &RelayConn{
		listener: l,
		wsConn:   ws,
		connGen:  gen,
		connID:   connID,
	}
	rc.readCond = sync.NewCond(&rc.readMu)
	return rc
}

func (c *RelayConn) kickAndClose() {
	c.closed.Store(true)
	c.remoteEOF.Store(true)
	c.readMu.Lock()
	c.readCond.Broadcast()
	c.readMu.Unlock()

	c.listener.mu.Lock()
	if c.listener.activeConn == c {
		c.listener.activeConn = nil
	}
	c.listener.mu.Unlock()

	c.listener.sendKickAgentForConn(c.wsConn, c.connGen, c.connID)
}

func (c *RelayConn) feed(payload []byte) {
	c.readMu.Lock()
	kick := c.feedLocked(payload)
	c.readMu.Unlock()

	if kick {
		c.listener.mu.Lock()
		if c.listener.activeConn == c {
			c.listener.activeConn = nil
		}
		c.listener.mu.Unlock()
		c.listener.sendKickAgentForConn(c.wsConn, c.connGen, c.connID)
	}
}

func (c *RelayConn) feedLocked(payload []byte) bool {
	if c.closed.Load() || c.remoteEOF.Load() {
		return false
	}

	cipher := c.connCipher.Load()
	if cipher == nil {
		agentNonce, err := e2ee.VerifyAgentHandshake(c.listener.handshakeKey, c.listener.sessionID, payload)
		if err != nil {
			c.closed.Store(true)
			c.remoteEOF.Store(true)
			c.readCond.Broadcast()
			return true
		}

		newCipher, err := e2ee.DeriveConnCipher(e2ee.RoleOperator, c.listener.sessionID, c.listener.secretKey, c.opNonce, agentNonce)
		if err != nil {
			c.closed.Store(true)
			c.remoteEOF.Store(true)
			c.readCond.Broadcast()
			return true
		}

		c.connCipher.Store(newCipher)
		return false
	}

	recType, plaintext, err := cipher.DecryptRecord(payload)
	if err != nil {
		c.closed.Store(true)
		c.remoteEOF.Store(true)
		c.readCond.Broadcast()
		return true
	}

	switch recType {
	case e2ee.RecordTypeEOF:
		c.remoteEOF.Store(true)
		c.readCond.Broadcast()
		return false
	case e2ee.RecordTypeData:
		// proceed to buffer below
	default:
		c.closed.Store(true)
		c.remoteEOF.Store(true)
		c.readCond.Broadcast()
		return true
	}

	if c.readBuf.Len()+len(plaintext) > maxReadBufferCap {
		c.closed.Store(true)
		c.remoteEOF.Store(true)
		c.readCond.Broadcast()
		return true
	}

	c.readBuf.Write(plaintext)
	if !bytes.HasSuffix(plaintext, []byte("\n")) {
		c.readBuf.WriteByte('\n')
	}
	c.readCond.Broadcast()
	return false
}

func (c *RelayConn) signalEOF() {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.remoteEOF.Store(true)
	c.readCond.Broadcast()
}

func (c *RelayConn) IsClosedOrEOF() bool {
	return c.closed.Load() || c.remoteEOF.Load()
}

func (c *RelayConn) Read(p []byte) (n int, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}

		if c.readBuf.Len() > 0 {
			return c.readBuf.Read(p)
		}

		if c.remoteEOF.Load() {
			return 0, io.EOF
		}

		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}

		var timer *time.Timer
		if !c.readDeadline.IsZero() {
			timeout := time.Until(c.readDeadline)
			if timeout <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.AfterFunc(timeout, func() {
				c.readMu.Lock()
				c.readCond.Broadcast()
				c.readMu.Unlock()
			})
		}

		c.readCond.Wait()
		if timer != nil {
			timer.Stop()
		}
	}
}

func (c *RelayConn) Write(p []byte) (n int, err error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	if c.writeBuf.Len()+len(p) > maxWriteBufferCap {
		return 0, errors.New("write buffer capacity exceeded")
	}

	c.writeBuf.Write(p)

	// Flush frame when a newline is encountered
	if bytes.Contains(c.writeBuf.Bytes(), []byte("\n")) {
		if err := c.flushLocked(); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func (c *RelayConn) flushLocked() error {
	data := c.writeBuf.Bytes()
	if len(data) == 0 {
		return nil
	}

	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		return os.ErrDeadlineExceeded
	}

	cipher := c.connCipher.Load()
	if cipher == nil {
		return errors.New("e2ee handshake not completed")
	}

	c.listener.mu.Lock()
	active := (c.listener.activeConn == c)
	c.listener.mu.Unlock()

	if !active || c.closed.Load() || c.remoteEOF.Load() {
		return net.ErrClosed
	}

	ciphertext, err := cipher.EncryptRecord(e2ee.RecordTypeData, data)
	if err != nil {
		return fmt.Errorf("failed to encrypt response frame: %w", err)
	}

	// Header: [ FrameData (0x01) | connID (8B) ] + ciphertext
	frame := make([]byte, 9+len(ciphertext))
	frame[0] = e2ee.FrameData
	binary.BigEndian.PutUint64(frame[1:9], c.connID)
	copy(frame[9:], ciphertext)

	if err := c.listener.writeWS(c.wsConn, c.connGen, websocket.BinaryMessage, frame, c.writeDeadline); err != nil {
		if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		return fmt.Errorf("failed to send frame to relay: %w", err)
	}

	c.writeBuf.Reset()
	return nil
}

func (c *RelayConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	c.readMu.Lock()
	c.readCond.Broadcast()
	c.readMu.Unlock()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// If remote agent hasn't disconnected yet, send authenticated EOF if cipher available
	if !c.remoteEOF.Load() {
		if cipher := c.connCipher.Load(); cipher != nil {
			eofRecord, err := cipher.EncryptRecord(e2ee.RecordTypeEOF, []byte{})
			if err == nil {
				frame := make([]byte, 9+len(eofRecord))
				frame[0] = e2ee.FrameData
				binary.BigEndian.PutUint64(frame[1:9], c.connID)
				copy(frame[9:], eofRecord)

				_ = c.listener.writeWS(c.wsConn, c.connGen, websocket.BinaryMessage, frame, time.Now().Add(writeWait))
			}
		}

		// Unconditionally send kick frame even if cipher == nil
		c.listener.sendKickAgentForConn(c.wsConn, c.connGen, c.connID)
	}

	c.listener.mu.Lock()
	if c.listener.activeConn == c {
		c.listener.activeConn = nil
	}
	c.listener.mu.Unlock()

	c.writeBuf.Reset()
	return nil
}

func (c *RelayConn) LocalAddr() net.Addr {
	return relayAddr{network: "relay", str: c.listener.sessionID}
}

func (c *RelayConn) RemoteAddr() net.Addr {
	return relayAddr{network: "relay", str: fmt.Sprintf("agent-%d", c.connID)}
}

func (c *RelayConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	_ = c.SetWriteDeadline(t)
	return nil
}

func (c *RelayConn) SetReadDeadline(t time.Time) error {
	c.readMu.Lock()
	c.readDeadline = t
	c.readCond.Broadcast()
	c.readMu.Unlock()
	return nil
}

func (c *RelayConn) SetWriteDeadline(t time.Time) error {
	c.writeMu.Lock()
	c.writeDeadline = t
	c.writeMu.Unlock()
	return nil
}
