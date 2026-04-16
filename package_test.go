package sftp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestPackUnpackRQ(t *testing.T) {
	fileinfo := []byte(`{"filename":"test.txt","filesize":100}`)
	opts := options{"blksize": "1024", "tsize": "5000"}

	buf := make([]byte, datagramLength)
	n := packRQ(buf, opWRQ, fileinfo, opts)

	opcode := binary.BigEndian.Uint16(buf[:2])
	if opcode != opWRQ {
		t.Errorf("opcode = %d, want %d", opcode, opWRQ)
	}

	gotInfo, gotOpts, err := unpackRQ(buf[:n])
	if err != nil {
		t.Fatalf("unpackRQ error: %v", err)
	}
	if !bytes.Equal(gotInfo, fileinfo) {
		t.Errorf("fileinfo = %q, want %q", gotInfo, fileinfo)
	}
	for k, v := range opts {
		if gotOpts[k] != v {
			t.Errorf("opts[%q] = %q, want %q", k, gotOpts[k], v)
		}
	}
}

func TestPackUnpackRQ_NoOpts(t *testing.T) {
	fileinfo := []byte("test.txt")
	buf := make([]byte, datagramLength)
	n := packRQ(buf, opRRQ, fileinfo, nil)

	gotInfo, gotOpts, err := unpackRQ(buf[:n])
	if err != nil {
		t.Fatalf("unpackRQ error: %v", err)
	}
	if !bytes.Equal(gotInfo, fileinfo) {
		t.Errorf("fileinfo = %q, want %q", gotInfo, fileinfo)
	}
	if gotOpts != nil {
		t.Errorf("opts = %v, want nil", gotOpts)
	}
}

func TestPackUnpackRQ_RRQ(t *testing.T) {
	fileinfo := []byte(`{"filename":"read.dat"}`)
	buf := make([]byte, datagramLength)
	n := packRQ(buf, opRRQ, fileinfo, nil)

	gotInfo, _, err := unpackRRQ(pRRQ(buf[:n]))
	if err != nil {
		t.Fatalf("unpackRRQ error: %v", err)
	}
	if !bytes.Equal(gotInfo, fileinfo) {
		t.Errorf("fileinfo = %q, want %q", gotInfo, fileinfo)
	}
}

func TestPackUnpackRQ_WRQ(t *testing.T) {
	fileinfo := []byte(`{"filename":"write.dat"}`)
	buf := make([]byte, datagramLength)
	n := packRQ(buf, opWRQ, fileinfo, nil)

	gotInfo, _, err := unpackWRQ(pWRQ(buf[:n]))
	if err != nil {
		t.Fatalf("unpackWRQ error: %v", err)
	}
	if !bytes.Equal(gotInfo, fileinfo) {
		t.Errorf("fileinfo = %q, want %q", gotInfo, fileinfo)
	}
}

func TestPackUnpackOACK(t *testing.T) {
	opts := options{"blksize": "1024"}
	buf := make([]byte, datagramLength)
	n := packOACK(buf, opts)

	// verify opcode is opOACK (6), not opACK (4)
	opcode := binary.BigEndian.Uint16(buf[:2])
	if opcode != opOACK {
		t.Errorf("opcode = %d, want %d (opOACK)", opcode, opOACK)
	}

	gotOpts, err := unpackOACK(pOACK(buf[:n]))
	if err != nil {
		t.Fatalf("unpackOACK error: %v", err)
	}
	if gotOpts["blksize"] != "1024" {
		t.Errorf("blksize = %q, want %q", gotOpts["blksize"], "1024")
	}
}

func TestPackERROR(t *testing.T) {
	buf := make([]byte, datagramLength)
	msg := "file not found"
	n := packERROR(buf, 2, msg)

	p := pERROR(buf[:n])
	if p.code() != 2 {
		t.Errorf("code = %d, want 2", p.code())
	}
	got := p.message()
	if len(got) == 0 || got[:len(msg)] != msg {
		t.Errorf("message = %q, want prefix %q", got, msg)
	}
}

func TestDataBlock(t *testing.T) {
	buf := make([]byte, datagramLength)
	initBlockHeader(buf, 42)

	opcode := binary.BigEndian.Uint16(buf[:2])
	if opcode != opDATA {
		t.Errorf("opcode = %d, want %d", opcode, opDATA)
	}
	p := pDATA(buf)
	if p.block() != 42 {
		t.Errorf("block = %d, want 42", p.block())
	}
}

func TestACKBlock(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint16(buf, opACK)
	binary.BigEndian.PutUint16(buf[2:], 7)

	p := pACK(buf)
	if p.block() != 7 {
		t.Errorf("block = %d, want 7", p.block())
	}
}

func TestParsePacket_AllTypes(t *testing.T) {
	tests := []struct {
		name  string
		build func() []byte
	}{
		{"RRQ", func() []byte {
			b := make([]byte, datagramLength)
			n := packRQ(b, opRRQ, []byte("f"), nil)
			return b[:n]
		}},
		{"WRQ", func() []byte {
			b := make([]byte, datagramLength)
			n := packRQ(b, opWRQ, []byte("f"), nil)
			return b[:n]
		}},
		{"DATA", func() []byte {
			b := make([]byte, 10)
			initBlockHeader(b, 1)
			return b
		}},
		{"ACK", func() []byte {
			b := make([]byte, 4)
			binary.BigEndian.PutUint16(b, opACK)
			binary.BigEndian.PutUint16(b[2:], 1)
			return b
		}},
		{"ERROR", func() []byte {
			b := make([]byte, datagramLength)
			n := packERROR(b, 1, "err")
			return b[:n]
		}},
		{"OACK", func() []byte {
			b := make([]byte, datagramLength)
			n := packOACK(b, options{"k": "v"})
			return b[:n]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parsePacket(tt.build())
			if err != nil {
				t.Fatalf("parsePacket error: %v", err)
			}
			if p == nil {
				t.Fatal("parsePacket returned nil")
			}
		})
	}
}

func TestParsePacket_ShortPacket(t *testing.T) {
	_, err := parsePacket([]byte{0})
	if err == nil {
		t.Error("expected error for short packet")
	}
}

func TestParsePacket_UnknownOpcode(t *testing.T) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint16(buf, 99)
	_, err := parsePacket(buf)
	if err == nil {
		t.Error("expected error for unknown opcode")
	}
}
