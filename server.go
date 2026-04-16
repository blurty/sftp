package sftp

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// NewServer creates SFTP server. It requires two functions to handle
// read and write requests.
// In case nil is provided for read or write handler the respective
// operation is disabled.
func NewServer(readHandler func(filename string, rf io.ReaderFrom) error,
	writeHandler func(filename string, wt io.WriterTo) error) *Server {
	return &Server{
		readHandler:  readHandler,
		writeHandler: writeHandler,
		timeout:      defaultTimeout,
		retries:      defaultRetries,
	}
}

type Server struct {
	readHandler  func(filename string, rf io.ReaderFrom) error
	writeHandler func(filename string, wt io.WriterTo) error
	backoff      backoffFunc
	conn         *net.UDPConn
	quit         chan chan struct{}
	wg           sync.WaitGroup
	timeout      time.Duration
	retries      int
}

// SetTimeout sets maximum time server waits for single network
// round-trip to succeed.
// Default is 5 seconds.
func (s *Server) SetTimeout(t time.Duration) {
	if t <= 0 {
		s.timeout = defaultTimeout
	} else {
		s.timeout = t
	}
}

// SetRetries sets maximum number of attempts server made to transmit a
// packet.
// Default is 5 attempts.
func (s *Server) SetRetries(count int) {
	if count < 1 {
		s.retries = defaultRetries
	} else {
		s.retries = count
	}
}

// SetBackoff sets a user provided function that is called to provide a
// backoff duration prior to retransmitting an unacknowledged packet.
func (s *Server) SetBackoff(h backoffFunc) {
	s.backoff = h
}

// ListenAndServe binds to address provided and start the server.
// ListenAndServe returns when Shutdown is called.
func (s *Server) ListenAndServe(addr string) error {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", a)
	if err != nil {
		return err
	}
	return s.Serve(conn)
}

// Serve starts server provided already opened UDP connecton. It is
// useful for the case when you want to run server in separate goroutine
// but still want to be able to handle any errors opening connection.
// Serve returns when Shutdown is called or connection is closed.
func (s *Server) Serve(conn *net.UDPConn) error {
	defer conn.Close()
	laddr := conn.LocalAddr()
	host, _, err := net.SplitHostPort(laddr.String())
	if err != nil {
		return err
	}
	s.conn = conn
	addr := net.ParseIP(host)
	if addr == nil {
		return fmt.Errorf("failed to determine IP class of listening address")
	}
	var conn4 *ipv4.PacketConn
	var conn6 *ipv6.PacketConn
	if addr.To4() != nil {
		conn4 = ipv4.NewPacketConn(conn)
		if err := conn4.SetControlMessage(ipv4.FlagDst, true); err != nil {
			conn4 = nil
		}
	} else {
		conn6 = ipv6.NewPacketConn(conn)
		if err := conn6.SetControlMessage(ipv6.FlagDst, true); err != nil {
			conn6 = nil
		}
	}
	s.quit = make(chan chan struct{})
	for {
		select {
		case q := <-s.quit:
			q <- struct{}{}
			return nil
		default:
			var err error
			if conn4 != nil {
				err = s.processRequest4(conn4)
			} else if conn6 != nil {
				err = s.processRequest6(conn6)
			} else {
				err = s.processRequest()
			}
			if err != nil {
				// TODO: add logging handler
			}
		}
	}
}

func (s *Server) processRequest4(conn4 *ipv4.PacketConn) error {
	buf := make([]byte, datagramLength)
	cnt, control, srcAddr, err := conn4.ReadFrom(buf)
	if err != nil {
		return fmt.Errorf("reading UDP: %v", err)
	}
	var localAddr net.IP
	if control != nil {
		localAddr = control.Dst
	}
	return s.handlePacket(localAddr, srcAddr.(*net.UDPAddr), buf, cnt)
}

func (s *Server) processRequest6(conn6 *ipv6.PacketConn) error {
	buf := make([]byte, datagramLength)
	cnt, control, srcAddr, err := conn6.ReadFrom(buf)
	if err != nil {
		return fmt.Errorf("reading UDP: %v", err)
	}
	var localAddr net.IP
	if control != nil {
		localAddr = control.Dst
	}
	return s.handlePacket(localAddr, srcAddr.(*net.UDPAddr), buf, cnt)
}

func (s *Server) processRequest() error {
	buf := make([]byte, datagramLength)
	cnt, srcAddr, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return fmt.Errorf("reading UDP: %v", err)
	}
	return s.handlePacket(nil, srcAddr, buf, cnt)
}

// Shutdown make server stop listening for new requests, allows
// server to finish outstanding transfers and stops server.
func (s *Server) Shutdown() {
	s.conn.Close()
	q := make(chan struct{})
	s.quit <- q
	<-q
	s.wg.Wait()
}

func (s *Server) handlePacket(localAddr net.IP, remoteAddr *net.UDPAddr, buffer []byte, n int) error {
	p, err := parsePacket(buffer[:n])
	if err != nil {
		return err
	}
	switch p := p.(type) {
	case pWRQ:
		fileinfo, opts, err := unpackRQ(p)
		if err != nil {
			return fmt.Errorf("unpack WRQ: %v", err)
		}
		var fi Filer
		if err := json.Unmarshal(fileinfo, &fi); err != nil {
			return fmt.Errorf("unmarshal file info: %v", err)
		}
		filename := fi.Filename
		conn, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			// --- server-side WRQ handshake ---
			proceed, err := s.doWRQHandshake(conn, remoteAddr, fi)
			if err != nil || !proceed {
				conn.Close()
				return
			}

			// handshake passed, create receiver and handle write
			// receiver's terminate() or abort() will close conn
			wt := &receiver{
				send:    make([]byte, datagramLength),
				receive: make([]byte, datagramLength),
				conn:    conn,
				retry:   &backoff{handler: s.backoff},
				timeout: s.timeout,
				retries: s.retries,
				addr:    remoteAddr,
				localIP: localAddr,
				opts:    opts,
			}
			if s.writeHandler != nil {
				err := s.writeHandler(filename, wt)
				if err != nil {
					wt.abort(err)
				} else {
					wt.terminate()
				}
			} else {
				wt.abort(fmt.Errorf("server does not support write requests"))
			}
		}()
	case pRRQ:
		fileinfo, opts, err := unpackRQ(p)
		if err != nil {
			return fmt.Errorf("unpack RRQ: %v", err)
		}
		var fi Filer
		if err := json.Unmarshal(fileinfo, &fi); err != nil {
			return fmt.Errorf("unmarshal file info: %v", err)
		}
		filename := fi.Filename
		conn, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			// --- server-side RRQ handshake ---
			proceed, err := s.doRRQHandshake(conn, remoteAddr, fi)
			if err != nil || !proceed {
				conn.Close()
				return
			}

			// handshake passed, create sender and handle read
			// sender's abort() will close conn
			rf := &sender{
				send:    make([]byte, datagramLength),
				receive: make([]byte, datagramLength),
				tid:     remoteAddr.Port,
				conn:    conn,
				retry:   &backoff{handler: s.backoff},
				timeout: s.timeout,
				retries: s.retries,
				addr:    remoteAddr,
				localIP: localAddr,
				opts:    opts,
			}
			if s.readHandler != nil {
				err := s.readHandler(filename, rf)
				if err != nil {
					rf.abort(err)
				}
			} else {
				rf.abort(fmt.Errorf("server does not support read requests"))
			}
		}()
	default:
		return fmt.Errorf("unexpected %T", p)
	}
	return nil
}

// doWRQHandshake performs the server-side WRQ handshake.
// It checks the local file status, sends a WRQ response with ACK code,
// and waits for client confirmation if needed.
// Returns (true, nil) if data transfer should proceed.
func (s *Server) doWRQHandshake(conn *net.UDPConn, remoteAddr *net.UDPAddr, clientFile Filer) (bool, error) {
	response := checkFileForWrite(clientFile)
	responseInfo, err := json.Marshal(response)
	if err != nil {
		return false, err
	}
	sendBuf := make([]byte, datagramLength)
	n := packRQ(sendBuf, opWRQ, responseInfo, nil)
	if _, err := conn.WriteToUDP(sendBuf[:n], remoteAddr); err != nil {
		return false, err
	}

	switch response.ACK {
	case ackNPermit, ackDir:
		return false, nil
	case ackSame:
		// file already exists and is identical, no transfer needed
		return false, nil
	case ackNExist:
		// file doesn't exist, client will start sending data
		return true, nil
	case ackNSame:
		// file differs, wait for client's confirmation with StartIndex
		recvBuf := make([]byte, datagramLength)
		conn.SetReadDeadline(time.Now().Add(s.timeout))
		cn, _, err := conn.ReadFromUDP(recvBuf)
		if err != nil {
			return false, fmt.Errorf("waiting for WRQ confirmation: %v", err)
		}
		cp, err := parsePacket(recvBuf[:cn])
		if err != nil {
			return false, err
		}
		if _, ok := cp.(pWRQ); !ok {
			return false, fmt.Errorf("expected WRQ confirmation, got %T", cp)
		}
		return true, nil
	}
	return false, fmt.Errorf("unexpected ACK code: %d", response.ACK)
}

// doRRQHandshake performs the server-side RRQ handshake.
// It checks the local file status, sends a RRQ response with ACK code,
// and waits for client confirmation if needed.
// Returns (true, nil) if data transfer should proceed.
func (s *Server) doRRQHandshake(conn *net.UDPConn, remoteAddr *net.UDPAddr, clientFile Filer) (bool, error) {
	response := checkFileForRead(clientFile)
	responseInfo, err := json.Marshal(response)
	if err != nil {
		return false, err
	}
	sendBuf := make([]byte, datagramLength)
	n := packRQ(sendBuf, opRRQ, responseInfo, nil)
	if _, err := conn.WriteToUDP(sendBuf[:n], remoteAddr); err != nil {
		return false, err
	}

	switch response.ACK {
	case ackNPermit, ackDir:
		return false, nil
	case ackNExist:
		return false, nil
	case ackSame:
		// file exists, wait for client's confirmation (may contain StartIndex)
		recvBuf := make([]byte, datagramLength)
		conn.SetReadDeadline(time.Now().Add(s.timeout))
		cn, _, err := conn.ReadFromUDP(recvBuf)
		if err != nil {
			return false, fmt.Errorf("waiting for RRQ confirmation: %v", err)
		}
		cp, err := parsePacket(recvBuf[:cn])
		if err != nil {
			return false, err
		}
		if _, ok := cp.(pRRQ); !ok {
			return false, fmt.Errorf("expected RRQ confirmation, got %T", cp)
		}
		return true, nil
	case ackNSame:
		// file differs from client's copy, wait for confirmation
		recvBuf := make([]byte, datagramLength)
		conn.SetReadDeadline(time.Now().Add(s.timeout))
		cn, _, err := conn.ReadFromUDP(recvBuf)
		if err != nil {
			return false, fmt.Errorf("waiting for RRQ confirmation: %v", err)
		}
		cp, err := parsePacket(recvBuf[:cn])
		if err != nil {
			return false, err
		}
		if _, ok := cp.(pRRQ); !ok {
			return false, fmt.Errorf("expected RRQ confirmation, got %T", cp)
		}
		return true, nil
	}
	return false, fmt.Errorf("unexpected ACK code: %d", response.ACK)
}
