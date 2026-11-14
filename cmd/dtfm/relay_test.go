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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iinozemtsev-lab/dtfm/pkg/e2ee"
)

func newRelayConn(l *RelayListener, connID uint64) *RelayConn {
	return newRelayConnForGen(l, l.wsConn, l.connGen, connID)
}

func TestRelayConnConcurrentOperationsAndDeadlines(t *testing.T) {
	sessionID := "test-concurrent-ops"
	rawKey, _, _ := e2ee.GenerateSessionKey()

	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)
	opNonce, _, _ := e2ee.BuildOperatorHandshake(handshakeKey, sessionID)
	agentNonce, _, _ := e2ee.BuildAgentHandshake(handshakeKey, sessionID)

	opCipher, _ := e2ee.DeriveConnCipher(e2ee.RoleOperator, sessionID, rawKey, opNonce, agentNonce)
	agentCipher, _ := e2ee.DeriveConnCipher(e2ee.RoleAgent, sessionID, rawKey, opNonce, agentNonce)

	mockListener := &RelayListener{
		sessionID: sessionID,
		doneCh:    make(chan struct{}),
	}

	conn := newRelayConn(mockListener, 1)
	conn.connCipher.Store(opCipher)

	// 1. Read deadline expiration returns os.ErrDeadlineExceeded
	conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	_, err := conn.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded on read timeout, got: %v", err)
	}

	// 2. Concurrent Read and Feed under -race
	var wg sync.WaitGroup
	var feedMu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b := make([]byte, 32)
			_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, _ = conn.Read(b)
		}()

		go func() {
			defer wg.Done()
			feedMu.Lock()
			defer feedMu.Unlock()
			record, err := agentCipher.EncryptRecord(e2ee.RecordTypeData, []byte("data\n"))
			if err == nil {
				conn.feed(record)
			}
		}()
	}
	wg.Wait()

	// 3. Close unblocks all readers with net.ErrClosed
	_ = conn.Close()
	_, err = conn.Read(buf)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed after Close(), got: %v", err)
	}
}

func TestRelayConnConcurrentWritesAndStaleConn(t *testing.T) {
	// Start a test HTTP server with WebSocket upgrade
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverReceivedFrames := make(chan []byte, 100)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			serverReceivedFrames <- msg
		}
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer wsConn.Close()

	sessionID := "stale-conn-test"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)
	opNonce, _, _ := e2ee.BuildOperatorHandshake(handshakeKey, sessionID)
	agentNonce, _, _ := e2ee.BuildAgentHandshake(handshakeKey, sessionID)

	cipher1, _ := e2ee.DeriveConnCipher(e2ee.RoleOperator, sessionID, rawKey, opNonce, agentNonce)
	cipher2, _ := e2ee.DeriveConnCipher(e2ee.RoleOperator, sessionID, rawKey, opNonce, agentNonce)

	listener := &RelayListener{
		wsConn:    wsConn,
		sessionID: sessionID,
		doneCh:    make(chan struct{}),
	}

	conn1 := newRelayConn(listener, 1)
	conn1.connCipher.Store(cipher1)

	conn2 := newRelayConn(listener, 2)
	conn2.connCipher.Store(cipher2)

	// Set conn1 as active
	listener.activeConn = conn1

	// Concurrent writes on conn1 must not panic gorilla/websocket
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = conn1.Write([]byte("message from conn1\n"))
		}()
	}
	wg.Wait()

	// Switch active connection to conn2
	listener.mu.Lock()
	listener.activeConn = conn2
	listener.mu.Unlock()

	// Stale write from conn1 must be refused with net.ErrClosed
	_, err = conn1.Write([]byte("stale write from old conn1\n"))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed on stale connection write, got: %v", err)
	}

	// Writes from active conn2 must succeed
	_, err = conn2.Write([]byte("valid write from conn2\n"))
	if err != nil {
		t.Fatalf("expected active conn2 write to succeed, got: %v", err)
	}
}

func TestRelayConnFeedErrorNoDeadlock(t *testing.T) {
	sessionID := "test-feed-deadlock"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)

	listener := &RelayListener{
		sessionID:    sessionID,
		handshakeKey: handshakeKey,
		doneCh:       make(chan struct{}),
	}

	// 1. Corrupted handshake must not deadlock c.readMu
	conn1 := newRelayConn(listener, 1)
	done1 := make(chan struct{})
	go func() {
		conn1.feed([]byte("corrupted-short-handshake"))
		close(done1)
	}()

	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("feed with corrupted handshake deadlocked")
	}

	// Read on conn1 should unblock immediately with net.ErrClosed
	buf := make([]byte, 16)
	_, err := conn1.Read(buf)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed after feed error, got: %v", err)
	}

	// 2. Tampered ciphertext after valid handshake must not deadlock
	opNonce, _, _ := e2ee.BuildOperatorHandshake(handshakeKey, sessionID)
	agentNonce, _, _ := e2ee.BuildAgentHandshake(handshakeKey, sessionID)
	opCipher, _ := e2ee.DeriveConnCipher(e2ee.RoleOperator, sessionID, rawKey, opNonce, agentNonce)

	conn2 := newRelayConn(listener, 2)
	conn2.connCipher.Store(opCipher)

	done2 := make(chan struct{})
	go func() {
		conn2.feed([]byte("tampered-encrypted-record"))
		close(done2)
	}()

	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("feed with tampered ciphertext deadlocked")
	}

	_, err = conn2.Read(buf)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed after tampered record feed, got: %v", err)
	}
}

func TestRelayConnFeedConcurrentWithCloseAndWrite(t *testing.T) {
	sessionID := "test-feed-concurrent"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)
	opNonce, _, _ := e2ee.BuildOperatorHandshake(handshakeKey, sessionID)
	_, agentPacket, _ := e2ee.BuildAgentHandshake(handshakeKey, sessionID)

	listener := &RelayListener{
		sessionID:    sessionID,
		secretKey:    rawKey,
		handshakeKey: handshakeKey,
		doneCh:       make(chan struct{}),
	}

	for iter := 0; iter < 20; iter++ {
		conn := newRelayConn(listener, uint64(iter+1))
		conn.opNonce = opNonce
		listener.activeConn = conn

		var wg sync.WaitGroup
		wg.Add(3)

		// Goroutine 1: Feed handshake
		go func() {
			defer wg.Done()
			conn.feed(agentPacket)
		}()

		// Goroutine 2: Concurrent write
		go func() {
			defer wg.Done()
			_, _ = conn.Write([]byte("concurrent payload\n"))
		}()

		// Goroutine 3: Concurrent Close
		go func() {
			defer wg.Done()
			_ = conn.Close()
		}()

		wg.Wait()
	}
}

func TestEffectiveHubURLDerivation(t *testing.T) {
	derive := func(relayURL string) string {
		u, err := url.Parse(relayURL)
		if err != nil {
			return ""
		}
		scheme := "https"
		if u.Scheme == "ws" {
			scheme = "http"
		}
		basePath := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/ws")
		return fmt.Sprintf("%s://%s%s", scheme, u.Host, basePath)
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"wss://relay.dtfm.dev/ws", "https://relay.dtfm.dev"},
		{"ws://localhost:8088/ws", "http://localhost:8088"},
		{"wss://example.com/dtfm/ws", "https://example.com/dtfm"},
		{"ws://example.com/sub/path/ws", "http://example.com/sub/path"},
	}

	for _, tt := range tests {
		got := derive(tt.input)
		if got != tt.expected {
			t.Errorf("derive(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestRelayConnFeedConcurrentWithListenerClose(t *testing.T) {
	sessionID := "test-feed-listener-close"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)

	for iter := 0; iter < 20; iter++ {
		listener := &RelayListener{
			sessionID:    sessionID,
			secretKey:    rawKey,
			handshakeKey: handshakeKey,
			doneCh:       make(chan struct{}),
		}

		conn := newRelayConn(listener, uint64(iter+1))
		listener.activeConn = conn

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: Feed corrupted payload triggering kickAndClose
		go func() {
			defer wg.Done()
			conn.feed([]byte("corrupted-payload-triggering-kick"))
		}()

		// Goroutine 2: Concurrent listener.Close()
		go func() {
			defer wg.Done()
			_ = listener.Close()
		}()

		wg.Wait()
	}
}

func TestRelayListenerAutoReconnectEndToEnd(t *testing.T) {
	sessionID := "test-auto-reconnect-e2e"
	rawKey, _, err := e2ee.GenerateSessionKey()
	if err != nil {
		t.Fatalf("GenerateSessionKey: %v", err)
	}
	handshakeKey, err := e2ee.HandshakeKey(rawKey, sessionID)
	if err != nil {
		t.Fatalf("HandshakeKey: %v", err)
	}
	expectedOpAuth, _, expectedAgentVerifier, err := e2ee.DeriveAuthTokens(rawKey, sessionID)
	if err != nil {
		t.Fatalf("DeriveAuthTokens: %v", err)
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var connCount atomic.Int32
	reconnectedCh := make(chan struct{})
	agentDone := make(chan error, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DTFM-Auth") != expectedOpAuth || r.Header.Get("X-DTFM-Agent-Verifier") != expectedAgentVerifier {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		n := connCount.Add(1)
		if n == 1 {
			// Simulate relay idle eviction / restart immediately after initial connect.
			_ = ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "idle timeout"),
				time.Now().Add(time.Second))
			return
		}

		// Second connection (same Magic Link / sessionID & auth headers).
		close(reconnectedCh)

		// Simulate an agent connecting with connID=1 after reconnect.
		connFrame := make([]byte, 9)
		connFrame[0] = e2ee.FrameAgentConnected
		binary.BigEndian.PutUint64(connFrame[1:9], 1)
		if err := ws.WriteMessage(websocket.BinaryMessage, connFrame); err != nil {
			agentDone <- err
			return
		}

		// Read operator handshake frame.
		_, msg, err := ws.ReadMessage()
		if err != nil {
			agentDone <- fmt.Errorf("read op handshake: %w", err)
			return
		}
		if len(msg) < 9 || msg[0] != e2ee.FrameData {
			agentDone <- fmt.Errorf("unexpected frame type %v", msg)
			return
		}
		opNonce, err := e2ee.VerifyOperatorHandshake(handshakeKey, sessionID, msg[9:])
		if err != nil {
			agentDone <- fmt.Errorf("VerifyOperatorHandshake: %w", err)
			return
		}

		// Send agent handshake frame.
		agentNonce, agentHandshake, err := e2ee.BuildAgentHandshake(handshakeKey, sessionID)
		if err != nil {
			agentDone <- err
			return
		}
		hsOut := make([]byte, 9+len(agentHandshake))
		hsOut[0] = e2ee.FrameData
		binary.BigEndian.PutUint64(hsOut[1:9], 1)
		copy(hsOut[9:], agentHandshake)
		if err := ws.WriteMessage(websocket.BinaryMessage, hsOut); err != nil {
			agentDone <- err
			return
		}

		agentCipher, err := e2ee.DeriveConnCipher(e2ee.RoleAgent, sessionID, rawKey, opNonce, agentNonce)
		if err != nil {
			agentDone <- err
			return
		}

		// Send encrypted request line from agent -> operator.
		reqCt, err := agentCipher.EncryptRecord(e2ee.RecordTypeData, []byte("ping-after-reconnect\n"))
		if err != nil {
			agentDone <- err
			return
		}
		reqFrame := make([]byte, 9+len(reqCt))
		reqFrame[0] = e2ee.FrameData
		binary.BigEndian.PutUint64(reqFrame[1:9], 1)
		copy(reqFrame[9:], reqCt)
		if err := ws.WriteMessage(websocket.BinaryMessage, reqFrame); err != nil {
			agentDone <- err
			return
		}

		// Read encrypted response line from operator -> agent.
		_, respMsg, err := ws.ReadMessage()
		if err != nil {
			agentDone <- fmt.Errorf("read op response: %w", err)
			return
		}
		recType, pt, err := agentCipher.DecryptRecord(respMsg[9:])
		if err != nil {
			agentDone <- fmt.Errorf("DecryptRecord: %w", err)
			return
		}
		if recType != e2ee.RecordTypeData || string(pt) != "pong-after-reconnect\n" {
			agentDone <- fmt.Errorf("unexpected response: type=%d pt=%q", recType, pt)
			return
		}
		agentDone <- nil

		// Hold generation-2 connection open until client closes it.
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	var logBuf bytes.Buffer
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 15 * time.Millisecond,
		LogWriter:  &logBuf,
	})
	if err != nil {
		t.Fatalf("DialRelayListenerWithOptions: %v", err)
	}
	defer rl.Close()

	select {
	case <-reconnectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnect")
	}

	conn, err := rl.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("conn.Read: %v", err)
	}
	if string(buf[:n]) != "ping-after-reconnect\n" {
		t.Fatalf("got %q, want %q", buf[:n], "ping-after-reconnect\n")
	}

	if _, err := conn.Write([]byte("pong-after-reconnect\n")); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	select {
	case err := <-agentDone:
		if err != nil {
			t.Fatalf("simulated agent failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for simulated agent")
	}

	_ = rl.Close()
	logs := logBuf.String()
	if strings.Count(logs, "Relay connection lost; reconnecting...") != 1 {
		t.Fatalf("expected exactly 1 lost log, got logs:\n%s", logs)
	}
	if strings.Count(logs, "Relay connection restored (session active).") != 1 {
		t.Fatalf("expected exactly 1 restored log, got logs:\n%s", logs)
	}
}

func TestRelayListenerGenerationIsolationOnConnIDReset(t *testing.T) {
	sessionID := "test-gen-isolation"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	handshakeKey, _ := e2ee.HandshakeKey(rawKey, sessionID)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var connCount atomic.Int32
	gen1DropCh := make(chan struct{})
	gen2Frames := make(chan []byte, 16)
	gen2Ready := make(chan struct{})

	handleHandshake := func(ws *websocket.Conn) error {
		connFrame := make([]byte, 9)
		connFrame[0] = e2ee.FrameAgentConnected
		binary.BigEndian.PutUint64(connFrame[1:9], 1) // connID == 1 in BOTH generations!
		if err := ws.WriteMessage(websocket.BinaryMessage, connFrame); err != nil {
			return err
		}
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if _, err := e2ee.VerifyOperatorHandshake(handshakeKey, sessionID, msg[9:]); err != nil {
			return err
		}
		_, agentHS, _ := e2ee.BuildAgentHandshake(handshakeKey, sessionID)
		hsOut := make([]byte, 9+len(agentHS))
		hsOut[0] = e2ee.FrameData
		binary.BigEndian.PutUint64(hsOut[1:9], 1)
		copy(hsOut[9:], agentHS)
		if err := ws.WriteMessage(websocket.BinaryMessage, hsOut); err != nil {
			return err
		}
		return nil
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		n := connCount.Add(1)
		if err := handleHandshake(ws); err != nil {
			return
		}
		if n == 1 {
			<-gen1DropCh
			return
		}
		close(gen2Ready)
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			gen2Frames <- msg
		}
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
		LogWriter:  io.Discard,
	})
	if err != nil {
		t.Fatalf("DialRelayListenerWithOptions: %v", err)
	}
	defer rl.Close()

	// Accept gen 1 conn (connID=1) and wait for handshake cipher to complete.
	conn1, err := rl.Accept()
	if err != nil {
		t.Fatalf("Accept conn1: %v", err)
	}
	rc1 := conn1.(*RelayConn)
	deadline := time.Now().Add(2 * time.Second)
	for rc1.connCipher.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if rc1.connCipher.Load() == nil {
		t.Fatal("conn1 cipher never established")
	}

	// Drop gen 1 WebSocket and wait for gen 2 reconnect + connID=1.
	close(gen1DropCh)
	select {
	case <-gen2Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for gen 2")
	}

	conn2, err := rl.Accept()
	if err != nil {
		t.Fatalf("Accept conn2: %v", err)
	}
	rc2 := conn2.(*RelayConn)
	deadline = time.Now().Add(2 * time.Second)
	for rc2.connCipher.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if rc2.connCipher.Load() == nil {
		t.Fatal("conn2 cipher never established")
	}

	// Stale conn1 (connID=1 from gen 1) Write and Close MUST NOT touch gen 2's wsConn.
	if _, err := conn1.Write([]byte("stale-gen1\n")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed from stale conn1.Write, got: %v", err)
	}
	_ = conn1.Close()

	// Fresh conn2 (connID=1 from gen 2) Write MUST succeed without any FrameKickAgent preceding it.
	if _, err := conn2.Write([]byte("fresh-gen2\n")); err != nil {
		t.Fatalf("conn2.Write failed: %v", err)
	}

	select {
	case frame := <-gen2Frames:
		if len(frame) < 9 || frame[0] != e2ee.FrameData {
			t.Fatalf("expected FrameData on gen2 from conn2, got ctrlType=%d (kick=%d)", frame[0], e2ee.FrameKickAgent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for conn2 frame on gen2")
	}
}

func TestRelayListenerPermanent403AbortsReconnect(t *testing.T) {
	sessionID := "test-perm-403"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	var dialAttempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := dialAttempts.Add(1)
		if n == 1 {
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_ = ws.Close()
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
		LogWriter:  io.Discard,
	})
	if err != nil {
		t.Fatalf("DialRelayListenerWithOptions: %v", err)
	}
	defer rl.Close()

	_, err = rl.Accept()
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("expected 403 Forbidden error from Accept, got: %v", err)
	}

	// Give a brief moment to ensure no extra retries happen after 403.
	time.Sleep(30 * time.Millisecond)
	if got := dialAttempts.Load(); got != 2 {
		t.Fatalf("expected exactly 2 dial attempts (initial + 1 permanent 403), got %d", got)
	}
}

func TestRelayListenerPostUpgradeClose4003AbortsReconnect(t *testing.T) {
	sessionID := "test-close-4003"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	var dialAttempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialAttempts.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(wsCloseCodeForbidden, "forbidden"),
			time.Now().Add(time.Second))
		_ = ws.Close()
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
		LogWriter:  io.Discard,
	})
	if err != nil {
		t.Fatalf("DialRelayListenerWithOptions: %v", err)
	}
	defer rl.Close()

	_, err = rl.Accept()
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("expected 403 Forbidden error from Accept, got: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	if got := dialAttempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 dial attempt (aborted on close 4003), got %d", got)
	}
}

func TestRelayListenerCloseCancelsBackoffAndInFlightDial(t *testing.T) {
	sessionID := "test-close-cancel"
	rawKey, _, _ := e2ee.GenerateSessionKey()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	t.Run("during_backoff_sleep", func(t *testing.T) {
		var attempts atomic.Int32
		dropped := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) == 1 {
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				_ = ws.Close()
				close(dropped)
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer ts.Close()

		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
		rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
			MinBackoff: 5 * time.Second,
			MaxBackoff: 5 * time.Second,
			LogWriter:  io.Discard,
		})
		if err != nil {
			t.Fatalf("DialRelayListenerWithOptions: %v", err)
		}

		<-dropped
		time.Sleep(15 * time.Millisecond) // enter 5s backoff sleep

		start := time.Now()
		_ = rl.Close()
		_, acceptErr := rl.Accept()
		elapsed := time.Since(start)

		if !errors.Is(acceptErr, net.ErrClosed) {
			t.Fatalf("expected net.ErrClosed, got: %v", acceptErr)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("Close/Accept took too long during backoff: %v", elapsed)
		}
	})

	t.Run("during_inflight_dial_context", func(t *testing.T) {
		var attempts atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_ = ws.Close()
		}))
		defer ts.Close()

		inFlightDial := make(chan struct{})
		customDialer := *websocket.DefaultDialer
		customDialer.HandshakeTimeout = 10 * time.Second
		customDialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if attempts.Add(1) == 1 {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			close(inFlightDial)
			<-ctx.Done()
			return nil, ctx.Err()
		}

		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
		rl, err := DialRelayListenerWithOptions(wsURL, sessionID, rawKey, &RelayOptions{
			MinBackoff: 1 * time.Millisecond,
			MaxBackoff: 5 * time.Millisecond,
			LogWriter:  io.Discard,
			Dialer:     &customDialer,
		})
		if err != nil {
			t.Fatalf("DialRelayListenerWithOptions: %v", err)
		}

		select {
		case <-inFlightDial:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for in-flight reconnect dial")
		}

		start := time.Now()
		_ = rl.Close()
		_, acceptErr := rl.Accept()
		elapsed := time.Since(start)

		if !errors.Is(acceptErr, net.ErrClosed) {
			t.Fatalf("expected net.ErrClosed, got: %v", acceptErr)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("Close/Accept took too long during in-flight DialContext: %v", elapsed)
		}
	})
}
