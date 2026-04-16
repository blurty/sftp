package sftp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultBlockNum  = 5
	maxBlockNum      = 10
	defaultBlockSize = 512
	minBlockSize     = 512
	maxBlockSize     = 65464
)

type blocker struct {
	id    uint16   // block id
	size  int      // actual data length in block (header + body)
	used  bool     // block in flight, waiting for ACK
	data  []byte   // data buffer: [opcode(2) + blockID(2) + body(blksize)]
	retry *backoff // retry state for this block
}

// Write implements io.Writer interface
func (b *blocker) Write(p []byte) (n int, err error) {
	initBlockHeader(b.data, b.id)
	n = copy(b.data[blockHeaderLength:], p)
	if n > 0 {
		b.used = true
		b.size = blockHeaderLength + n
	}
	return n, nil
}

// ReadFrom implements io.ReaderFrom, reads one block of data from r
func (b *blocker) ReadFrom(r io.Reader) (n int64, err error) {
	initBlockHeader(b.data, b.id)
	bn, err := r.Read(b.data[blockHeaderLength:])
	b.size = blockHeaderLength + bn // always update size to avoid stale data
	if bn > 0 {
		b.used = true
	}
	return int64(bn), err
}

var (
	errNoPermission   = errors.New("no permission to read or write file")
	errFileNotExist   = errors.New("file does not exist")
	errFileNotSame    = errors.New("file not same")
	errServerInternal = errors.New("server internal error occurred")
	errTimeout        = errors.New("handle transmission timeout")
)

type sender struct {
	mu           sync.Mutex
	blockNum     int // number of blocks in the sliding window
	blocks       []*blocker
	conn         *net.UDPConn
	addr         *net.UDPAddr
	localIP      net.IP
	send         []byte // buffer for control packets (handshake, error, OACK)
	receive      []byte // buffer for received packets
	receivedSize int
	tid          int
	retry        *backoff
	timeout      time.Duration
	retries      int
	opts         options
	file         *Filer
	opcode       uint16
	done         chan struct{} // signal to stop background goroutines
}

func (s *sender) setBlockNum(num int) {
	if num < 1 || num > maxBlockNum {
		num = defaultBlockNum
	}
	s.blockNum = num
	s.blocks = make([]*blocker, num)
	for i := 0; i < num; i++ {
		s.blocks[i] = &blocker{
			retry: &backoff{},
		}
	}
}

func (s *sender) setBlockSize(blksize int) {
	if blksize < minBlockSize || blksize > maxBlockSize {
		blksize = defaultBlockSize
	}
	for i := 0; i < s.blockNum; i++ {
		s.blocks[i].data = make([]byte, blksize+blockHeaderLength)
	}
}

func (s *sender) SetSize(n int64) {
	if s.opts != nil {
		if _, ok := s.opts["tsize"]; ok {
			s.opts["tsize"] = strconv.FormatInt(n, 10)
		}
	}
}

// shakeHands establishes a connection with the server (client side).
// It sends a RRQ/WRQ request and processes the server's response.
func (s *sender) shakeHands() error {
	info, err := json.Marshal(s.file)
	if err != nil {
		return err
	}
	n := packRQ(s.send, s.opcode, info, s.opts)
	s.retry.reset()
	for {
		s.sendDatagram(s.send[:n])
		err := s.recvDatagram()
		if err != nil {
			if s.retry.count() < s.retries {
				s.retry.backoff()
				continue
			}
			return err
		}
		// parse hand-shake response
		res, err := parsePacket(s.receive[:s.receivedSize])
		if err != nil {
			return err
		}
		if s.opcode == opRRQ {
			ackData, ok := res.(pRRQ)
			if !ok {
				s.retry.backoff()
				continue
			}
			fdata, opts, err := unpackRRQ(ackData)
			if err != nil {
				return err
			}
			s.dealOpts(opts)
			var fi Filer
			if err = json.Unmarshal(fdata, &fi); err != nil {
				return err
			}
			if fi.Filename != s.file.Filename {
				return errServerInternal
			}
			switch fi.ACK {
			case ackNPermit:
				s.file.State = stateNO
				return errNoPermission
			case ackNExist:
				s.file.State = stateNO
				return errFileNotExist
			case ackNSame:
				s.file.State = stateNO
				info, _ = json.Marshal(s.file)
				n = packRQ(s.send, s.opcode, info, nil)
				s.sendDatagram(s.send[:n])
				return errFileNotSame
			case ackSame:
				s.file.State = stateYES
				info, _ = json.Marshal(s.file)
				n = packRQ(s.send, s.opcode, info, nil)
				s.sendDatagram(s.send[:n])
				return nil
			}
		} else if s.opcode == opWRQ {
			ackData, ok := res.(pWRQ)
			if !ok {
				s.retry.backoff()
				continue
			}
			fdata, opts, err := unpackWRQ(ackData)
			if err != nil {
				return err
			}
			s.dealOpts(opts)
			var fi Filer
			if err = json.Unmarshal(fdata, &fi); err != nil {
				return err
			}
			if fi.Filename != s.file.Filename {
				return errServerInternal
			}
			switch fi.ACK {
			case ackNPermit:
				s.file.State = stateNO
				return errNoPermission
			case ackNExist:
				s.file.State = stateYES
				return nil
			case ackNSame:
				s.file.State = stateYES
				if isHalfFiler(*s.file, fi) {
					s.file.StartIndex = fi.FileSize + 1
				} else {
					s.file.StartIndex = 0
				}
				info, _ = json.Marshal(s.file)
				n = packRQ(s.send, s.opcode, info, nil)
				s.sendDatagram(s.send[:n])
				return nil
			case ackSame:
				s.file.State = stateComplete
				return nil
			}
		} else {
			return errServerInternal
		}
		return nil
	}
}

// sendContents reads file data and sends blocks with a sliding window.
func (s *sender) sendContents() error {
	fp, err := os.Open(s.file.Filename)
	if err != nil {
		return err
	}
	defer fp.Close()

	// seek to start position for resume
	if s.file.StartIndex > 0 {
		if _, err := fp.Seek(s.file.StartIndex, io.SeekStart); err != nil {
			return err
		}
	}

	return s.sendFromReader(fp)
}

// sendFromReader reads data from r and sends it in blocks with ACK handling.
func (s *sender) sendFromReader(r io.Reader) error {
	s.done = make(chan struct{})
	var recvErr error
	var wg sync.WaitGroup

	// start ACK receiver goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		recvErr = s.recvACKLoop()
	}()

	var blockID uint16 = 1
	eof := false

	for !eof {
		sent := false
		for i := 0; i < s.blockNum; i++ {
			s.mu.Lock()
			used := s.blocks[i].used
			s.mu.Unlock()
			if used {
				continue
			}

			s.blocks[i].id = blockID
			_, err := s.blocks[i].ReadFrom(r)
			if err == io.EOF {
				// EOF returned: block may have 0 or more data bytes.
				// In TFTP, a block shorter than blksize signals EOF.
				s.blocks[i].used = true
				s.blocks[i].retry.reset()
				s.sendOneBlock(i)
				blockID++
				eof = true
				break
			} else if err != nil {
				close(s.done)
				wg.Wait()
				return err
			}
			s.blocks[i].retry.reset()
			s.sendOneBlock(i)
			blockID++
			sent = true
			// In TFTP, a short block (data < blksize) signals EOF.
			if s.blocks[i].size < len(s.blocks[i].data) {
				eof = true
				break
			}
		}
		if !sent && !eof {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// wait for all in-flight blocks to be ACKed
	deadline := time.After(time.Duration(s.retries+1) * s.timeout)
	for {
		allDone := true
		s.mu.Lock()
		for i := 0; i < s.blockNum; i++ {
			if s.blocks[i].used {
				allDone = false
				break
			}
		}
		s.mu.Unlock()
		if allDone {
			break
		}
		select {
		case <-deadline:
			close(s.done)
			wg.Wait()
			return errTimeout
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	close(s.done)
	wg.Wait()
	if recvErr != nil {
		return recvErr
	}
	return nil
}

// sendOneBlock sends a single data block to the remote peer.
func (s *sender) sendOneBlock(i int) {
	s.conn.WriteToUDP(s.blocks[i].data[:s.blocks[i].size], s.addr)
}

// recvACKLoop receives ACK packets and marks blocks as free.
// It also handles retransmission on timeout.
func (s *sender) recvACKLoop() error {
	for {
		select {
		case <-s.done:
			return nil
		default:
		}

		err := s.conn.SetReadDeadline(time.Now().Add(s.timeout))
		if err != nil {
			return err
		}
		n, addr, err := s.conn.ReadFromUDP(s.receive)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.retransmitBlocks()
				continue
			}
			// check if we were told to stop
			select {
			case <-s.done:
				return nil
			default:
			}
			continue
		}
		if !addr.IP.Equal(s.addr.IP) || (s.tid != 0 && addr.Port != s.tid) {
			continue
		}
		s.tid = addr.Port

		res, err := parsePacket(s.receive[:n])
		if err != nil {
			continue
		}

		switch p := res.(type) {
		case pACK:
			id := p.block()
			s.mu.Lock()
			for i := 0; i < s.blockNum; i++ {
				if s.blocks[i].used && s.blocks[i].id == id {
					s.blocks[i].used = false
					break
				}
			}
			s.mu.Unlock()
		case pERROR:
			return fmt.Errorf("remote error: code=%d, message=%s", p.code(), p.message())
		}
	}
}

// retransmitBlocks resends all in-flight blocks that haven't been ACKed.
func (s *sender) retransmitBlocks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.blockNum; i++ {
		if s.blocks[i].used {
			if s.blocks[i].retry.count() < s.retries {
				s.blocks[i].retry.attemp++
				s.conn.WriteToUDP(s.blocks[i].data[:s.blocks[i].size], s.addr)
			}
		}
	}
}

// dealOpts processes negotiated options from the remote peer.
func (s *sender) dealOpts(opts options) {
	if opts == nil {
		return
	}
	for name, value := range opts {
		switch name {
		case "blksize":
			blksize, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			s.setBlockSize(blksize)
		case "tsize":
			if s.opts == nil {
				s.opts = make(options)
			}
			s.opts["tsize"] = value
		}
	}
}

func (s *sender) sendRQ() error {
	info, err := json.Marshal(s.file)
	if err != nil {
		return err
	}
	n := packRQ(s.send, s.opcode, info, s.opts)
	s.sendDatagram(s.send[:n])
	return nil
}

func (s *sender) sendOptions() error {
	for name, value := range s.opts {
		if name == "blksize" {
			blksize, err := strconv.Atoi(value)
			if err != nil {
				delete(s.opts, name)
				continue
			}
			s.setBlockSize(blksize)
		} else if name == "tsize" {
			if value == "0" {
				delete(s.opts, name)
			}
		} else {
			delete(s.opts, name)
		}
	}
	if len(s.opts) > 0 {
		m := packOACK(s.send, s.opts)
		s.retry.reset()
		for {
			err := s.sendDatagram(s.send[:m])
			if err != nil {
				return err
			}
			err = s.recvDatagram()
			if err == nil {
				p, err := parsePacket(s.receive[:s.receivedSize])
				if err == nil {
					if pack, ok := p.(pOACK); ok {
						opts, err := unpackOACK(pack)
						if err != nil {
							s.abort(err)
							return err
						}
						for name, value := range opts {
							if name == "blksize" {
								blksize, err := strconv.Atoi(value)
								if err != nil {
									continue
								}
								s.setBlockSize(blksize)
							}
						}
						return nil
					}
				}
			}
			if s.retry.count() < s.retries {
				s.retry.backoff()
				continue
			}
			return err
		}
	}
	return nil
}

// ReadFrom reads from r and sends data blocks to the remote peer.
// This is used on the server side to handle RRQ (read requests).
func (s *sender) ReadFrom(r io.Reader) (int64, error) {
	if s.opts != nil {
		err := s.sendOptions()
		if err != nil {
			s.abort(err)
			return 0, err
		}
	}
	s.setBlockNum(defaultBlockNum)
	s.setBlockSize(defaultBlockSize)

	s.done = make(chan struct{})
	var totalSent int64
	var recvErr error
	var wg sync.WaitGroup

	// start ACK receiver
	wg.Add(1)
	go func() {
		defer wg.Done()
		recvErr = s.recvACKLoop()
	}()

	var blockID uint16 = 1
	eof := false

	for !eof {
		sent := false
		for i := 0; i < s.blockNum; i++ {
			s.mu.Lock()
			used := s.blocks[i].used
			s.mu.Unlock()
			if used {
				continue
			}

			s.blocks[i].id = blockID
			n, err := s.blocks[i].ReadFrom(r)
			totalSent += n
			if err == io.EOF {
				s.blocks[i].used = true
				s.blocks[i].retry.reset()
				s.sendOneBlock(i)
				blockID++
				eof = true
				break
			} else if err != nil {
				close(s.done)
				wg.Wait()
				s.abort(err)
				return totalSent, err
			}
			s.blocks[i].retry.reset()
			s.sendOneBlock(i)
			blockID++
			sent = true
			// In TFTP, a short block (data < blksize) signals EOF.
			if s.blocks[i].size < len(s.blocks[i].data) {
				eof = true
				break
			}
		}
		if !sent && !eof {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// wait for all in-flight blocks to be ACKed
	deadline := time.After(time.Duration(s.retries+1) * s.timeout)
	for {
		allDone := true
		s.mu.Lock()
		for i := 0; i < s.blockNum; i++ {
			if s.blocks[i].used {
				allDone = false
				break
			}
		}
		s.mu.Unlock()
		if allDone {
			break
		}
		select {
		case <-deadline:
			close(s.done)
			wg.Wait()
			return totalSent, errTimeout
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	close(s.done)
	wg.Wait()
	if recvErr != nil {
		return totalSent, recvErr
	}
	return totalSent, nil
}

func (s *sender) recvDatagram() error {
	err := s.conn.SetReadDeadline(time.Now().Add(s.timeout))
	if err != nil {
		return err
	}
	n, addr, err := s.conn.ReadFromUDP(s.receive)
	if err != nil {
		return err
	}
	if !addr.IP.Equal(s.addr.IP) || (s.tid != 0 && addr.Port != s.tid) {
		return fmt.Errorf("datagram not wanted")
	}
	s.tid = addr.Port
	s.addr = addr // update to server's new transfer address (TID)
	s.receivedSize = n
	return nil
}

func (s *sender) sendDatagram(data []byte) error {
	_, err := s.conn.WriteToUDP(data, s.addr)
	return err
}

func (s *sender) abort(err error) error {
	if s.conn == nil {
		return nil
	}
	defer func() {
		s.conn.Close()
		s.conn = nil
	}()
	n := packERROR(s.send, 1, err.Error())
	_, wErr := s.conn.WriteToUDP(s.send[:n], s.addr)
	return wErr
}
