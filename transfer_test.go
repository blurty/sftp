package sftp

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// startTestServer creates and starts a server on 127.0.0.1 with a random port.
// Returns the address string and a shutdown function.
func startTestServer(t *testing.T,
	readHandler func(string, io.ReaderFrom) error,
	writeHandler func(string, io.WriterTo) error,
) (addr string, shutdown func()) {
	t.Helper()
	s := NewServer(readHandler, writeHandler)
	s.SetTimeout(5 * time.Second)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr = conn.LocalAddr().String()
	go s.Serve(conn)
	time.Sleep(50 * time.Millisecond) // wait for Serve to initialize quit channel
	return addr, func() { s.Shutdown() }
}

// TestHandshake_FileAlreadyExists tests the WRQ handshake when the file
// already exists on the server (same machine). Server returns ackSame,
// client returns immediately with no data transfer.
func TestHandshake_FileAlreadyExists(t *testing.T) {
	srcFile := createTempFile(t, []byte("hello world"))

	transferCalled := make(chan struct{}, 1)
	writeHandler := func(filename string, wt io.WriterTo) error {
		transferCalled <- struct{}{}
		var buf bytes.Buffer
		wt.WriteTo(&buf)
		return nil
	}

	addr, shutdown := startTestServer(t, nil, writeHandler)
	defer shutdown()

	client, err := NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	client.SetTimeout(2 * time.Second)
	client.SetRetries(3)

	err = client.SendFile(srcFile)
	if err != nil {
		t.Fatalf("SendFile error: %v", err)
	}

	// writeHandler should NOT be called because server returns ackSame
	select {
	case <-transferCalled:
		t.Error("writeHandler was called, but expected ackSame (no transfer)")
	case <-time.After(1 * time.Second):
		// expected: no transfer happened
	}
}

// TestTransfer_SmallFile sends a small file (< 1 block) through the full
// protocol path by directly using sender with a non-existent filename
// so the server returns ackNExist and proceeds to receive data.
func TestTransfer_SmallFile(t *testing.T) {
	testData := []byte("hello sftp transfer test")
	received := make(chan []byte, 1)

	writeHandler := func(filename string, wt io.WriterTo) error {
		var buf bytes.Buffer
		_, err := wt.WriteTo(&buf)
		if err != nil {
			return err
		}
		received <- buf.Bytes()
		return nil
	}

	addr, shutdown := startTestServer(t, nil, writeHandler)
	defer shutdown()

	serverAddr, _ := net.ResolveUDPAddr("udp", addr)
	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}

	sndr := &sender{
		send:    make([]byte, datagramLength),
		receive: make([]byte, datagramLength),
		conn:    clientConn,
		retry:   &backoff{},
		timeout: 5 * time.Second,
		retries: 5,
		addr:    serverAddr,
		file:    &Filer{Filename: "/nonexistent/small.dat", State: stateSync},
		opcode:  opWRQ,
	}
	sndr.setBlockNum(0)
	sndr.setBlockSize(0)

	if err := sndr.shakeHands(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := sndr.sendFromReader(bytes.NewReader(testData)); err != nil {
		t.Fatalf("sendFromReader: %v", err)
	}

	select {
	case data := <-received:
		if !bytes.Equal(data, testData) {
			t.Errorf("data mismatch:\n  got:  %q\n  want: %q", data, testData)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for transfer")
	}
}

// TestTransfer_MultiBlock sends data larger than one block (512 bytes)
// to exercise the sliding window mechanism.
func TestTransfer_MultiBlock(t *testing.T) {
	testData := bytes.Repeat([]byte("x"), 5000) // ~10 blocks
	received := make(chan []byte, 1)

	writeHandler := func(filename string, wt io.WriterTo) error {
		var buf bytes.Buffer
		_, err := wt.WriteTo(&buf)
		if err != nil {
			return err
		}
		received <- buf.Bytes()
		return nil
	}

	addr, shutdown := startTestServer(t, nil, writeHandler)
	defer shutdown()

	serverAddr, _ := net.ResolveUDPAddr("udp", addr)
	clientConn, _ := net.ListenUDP("udp", &net.UDPAddr{})

	sndr := &sender{
		send:    make([]byte, datagramLength),
		receive: make([]byte, datagramLength),
		conn:    clientConn,
		retry:   &backoff{},
		timeout: 5 * time.Second,
		retries: 5,
		addr:    serverAddr,
		file:    &Filer{Filename: "/nonexistent/multi.dat", State: stateSync},
		opcode:  opWRQ,
	}
	sndr.setBlockNum(0)
	sndr.setBlockSize(0)

	if err := sndr.shakeHands(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := sndr.sendFromReader(bytes.NewReader(testData)); err != nil {
		t.Fatalf("sendFromReader: %v", err)
	}

	select {
	case data := <-received:
		if !bytes.Equal(data, testData) {
			t.Errorf("data mismatch: got %d bytes, want %d bytes", len(data), len(testData))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for transfer")
	}
}

// TestTransfer_ExactBlockSize tests the edge case where the file size
// is an exact multiple of the block size (512 bytes). The sender must
// send a 0-data final block to signal EOF.
func TestTransfer_ExactBlockSize(t *testing.T) {
	testData := bytes.Repeat([]byte("y"), 512) // exactly 1 full block
	received := make(chan []byte, 1)

	writeHandler := func(filename string, wt io.WriterTo) error {
		var buf bytes.Buffer
		_, err := wt.WriteTo(&buf)
		if err != nil {
			return err
		}
		received <- buf.Bytes()
		return nil
	}

	addr, shutdown := startTestServer(t, nil, writeHandler)
	defer shutdown()

	serverAddr, _ := net.ResolveUDPAddr("udp", addr)
	clientConn, _ := net.ListenUDP("udp", &net.UDPAddr{})

	sndr := &sender{
		send:    make([]byte, datagramLength),
		receive: make([]byte, datagramLength),
		conn:    clientConn,
		retry:   &backoff{},
		timeout: 5 * time.Second,
		retries: 5,
		addr:    serverAddr,
		file:    &Filer{Filename: "/nonexistent/exact.dat", State: stateSync},
		opcode:  opWRQ,
	}
	sndr.setBlockNum(0)
	sndr.setBlockSize(0)

	if err := sndr.shakeHands(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := sndr.sendFromReader(bytes.NewReader(testData)); err != nil {
		t.Fatalf("sendFromReader: %v", err)
	}

	select {
	case data := <-received:
		if !bytes.Equal(data, testData) {
			t.Errorf("data mismatch: got %d bytes, want %d bytes", len(data), len(testData))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for transfer (exact block size EOF bug?)")
	}
}

// TestTransfer_EmptyData tests sending zero bytes of data.
func TestTransfer_EmptyData(t *testing.T) {
	received := make(chan []byte, 1)

	writeHandler := func(filename string, wt io.WriterTo) error {
		var buf bytes.Buffer
		_, err := wt.WriteTo(&buf)
		if err != nil {
			return err
		}
		received <- buf.Bytes()
		return nil
	}

	addr, shutdown := startTestServer(t, nil, writeHandler)
	defer shutdown()

	serverAddr, _ := net.ResolveUDPAddr("udp", addr)
	clientConn, _ := net.ListenUDP("udp", &net.UDPAddr{})

	sndr := &sender{
		send:    make([]byte, datagramLength),
		receive: make([]byte, datagramLength),
		conn:    clientConn,
		retry:   &backoff{},
		timeout: 5 * time.Second,
		retries: 5,
		addr:    serverAddr,
		file:    &Filer{Filename: "/nonexistent/empty.dat", State: stateSync},
		opcode:  opWRQ,
	}
	sndr.setBlockNum(0)
	sndr.setBlockSize(0)

	if err := sndr.shakeHands(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := sndr.sendFromReader(bytes.NewReader(nil)); err != nil {
		t.Fatalf("sendFromReader: %v", err)
	}

	select {
	case data := <-received:
		if len(data) != 0 {
			t.Errorf("expected empty data, got %d bytes", len(data))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for empty transfer")
	}
}

// TestTransfer_HandshakeTimeout tests that the client times out
// when no server is listening.
func TestTransfer_HandshakeTimeout(t *testing.T) {
	// connect to a port where nothing is listening
	client, err := NewClient("127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	client.SetTimeout(200 * time.Millisecond)
	client.SetRetries(2)

	srcFile := createTempFile(t, []byte("timeout test"))
	err = client.SendFile(srcFile)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}
