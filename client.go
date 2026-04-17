package sftp

import (
	"fmt"
	"net"
	"time"
)

type Client struct {
	addr    *net.UDPAddr
	timeout time.Duration
	retries int
	backoff backoffFunc
}

func NewClient(addr string) (*Client, error) {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolving address %s: %v", addr, err)
	}
	return &Client{
		addr:    a,
		timeout: defaultTimeout,
		retries: defaultRetries,
	}, nil
}

// RecvFile downloads a file from the server.
// TODO: implement RRQ-based file download
func (c *Client) RecvFile(filename string) error {
	return fmt.Errorf("RecvFile not implemented yet")
}

// SendFile sends a local file to the server.
func (c *Client) SendFile(filename string) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return err
	}
	flr, err := NewFiler(filename)
	if err != nil {
		conn.Close()
		return err
	}
	s := &sender{
		send:    make([]byte, datagramLength),
		receive: make([]byte, datagramLength),
		conn:    conn,
		retry:   &backoff{handler: c.backoff},
		timeout: c.timeout,
		retries: c.retries,
		addr:    c.addr,
		file:    flr,
		opcode:  opWRQ,
	}
	s.setBlockNum(0)  // will use defaultBlockNum
	s.setBlockSize(0) // will use defaultBlockSize
	err = s.shakeHands()
	if err != nil {
		conn.Close()
		return err
	}
	// file already exists and is identical on server
	if s.file.State == stateComplete {
		conn.Close()
		return nil
	}
	// sendContents -> sendFromReader will use conn; abort() closes it on error
	err = s.sendContents()
	conn.Close()
	return err
}

// SetTimeout sets maximum time client waits for single network round-trip to succeed.
// Default is 5 seconds.
func (c *Client) SetTimeout(t time.Duration) {
	if t <= 0 {
		c.timeout = defaultTimeout
	} else {
		c.timeout = t
	}
}

// SetRetries sets maximum number of attempts client made to transmit a packet.
// Default is 5 attempts.
func (c *Client) SetRetries(count int) {
	if count < 1 {
		c.retries = defaultRetries
	} else {
		c.retries = count
	}
}

// SetBackoff sets a user provided function that is called to provide a
// backoff duration prior to retransmitting an unacknowledged packet.
func (c *Client) SetBackoff(h backoffFunc) {
	c.backoff = h
}
