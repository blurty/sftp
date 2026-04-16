package sftp

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// eofReader returns data and io.EOF simultaneously on the first Read.
type eofReader struct {
	data []byte
	read bool
}

func (r *eofReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	n := copy(p, r.data)
	return n, io.EOF
}

func TestBlockerReadFrom(t *testing.T) {
	b := &blocker{
		id:    1,
		data:  make([]byte, defaultBlockSize+blockHeaderLength),
		retry: &backoff{},
	}

	data := []byte("hello world")
	n, err := b.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom error: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("n = %d, want %d", n, len(data))
	}
	if !b.used {
		t.Error("used = false, want true")
	}
	if b.size != blockHeaderLength+len(data) {
		t.Errorf("size = %d, want %d", b.size, blockHeaderLength+len(data))
	}
	if binary.BigEndian.Uint16(b.data[:2]) != opDATA {
		t.Error("opcode not opDATA")
	}
	if binary.BigEndian.Uint16(b.data[2:4]) != 1 {
		t.Error("block id not 1")
	}
	if !bytes.Equal(b.data[blockHeaderLength:blockHeaderLength+len(data)], data) {
		t.Error("data content mismatch")
	}
}

func TestBlockerReadFrom_EOFWithData(t *testing.T) {
	b := &blocker{
		id:    5,
		data:  make([]byte, defaultBlockSize+blockHeaderLength),
		retry: &backoff{},
	}

	data := []byte("partial")
	n, err := b.ReadFrom(&eofReader{data: data})
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if n != int64(len(data)) {
		t.Errorf("n = %d, want %d", n, len(data))
	}
	if !b.used {
		t.Error("used = false, want true (data was read)")
	}
	if b.size != blockHeaderLength+len(data) {
		t.Errorf("size = %d, want %d", b.size, blockHeaderLength+len(data))
	}
}

func TestBlockerReadFrom_EmptyEOF(t *testing.T) {
	b := &blocker{
		id:    1,
		data:  make([]byte, defaultBlockSize+blockHeaderLength),
		retry: &backoff{},
	}

	n, err := b.ReadFrom(&eofReader{data: nil})
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if b.used {
		t.Error("used = true, want false (no data)")
	}
	if b.size != blockHeaderLength {
		t.Errorf("size = %d, want %d (header only)", b.size, blockHeaderLength)
	}
}

func TestBlockerWrite(t *testing.T) {
	b := &blocker{
		id:    3,
		data:  make([]byte, defaultBlockSize+blockHeaderLength),
		retry: &backoff{},
	}

	data := []byte("write test")
	n, err := b.Write(data)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Errorf("n = %d, want %d", n, len(data))
	}
	if !b.used {
		t.Error("used = false, want true")
	}
	if b.size != blockHeaderLength+len(data) {
		t.Errorf("size = %d, want %d", b.size, blockHeaderLength+len(data))
	}
}

func TestSetBlockNum(t *testing.T) {
	s := &sender{}

	// 0 should use default
	s.setBlockNum(0)
	if s.blockNum != defaultBlockNum {
		t.Errorf("blockNum = %d, want %d", s.blockNum, defaultBlockNum)
	}
	for i := 0; i < s.blockNum; i++ {
		if s.blocks[i] == nil {
			t.Errorf("blocks[%d] is nil", i)
		}
	}

	// over max should use default
	s.setBlockNum(100)
	if s.blockNum != defaultBlockNum {
		t.Errorf("blockNum = %d, want %d", s.blockNum, defaultBlockNum)
	}

	// valid value
	s.setBlockNum(3)
	if s.blockNum != 3 {
		t.Errorf("blockNum = %d, want 3", s.blockNum)
	}
}

func TestSetBlockSize(t *testing.T) {
	s := &sender{}
	s.setBlockNum(2)

	// 0 should use default
	s.setBlockSize(0)
	for i := 0; i < s.blockNum; i++ {
		expected := defaultBlockSize + blockHeaderLength
		if len(s.blocks[i].data) != expected {
			t.Errorf("blocks[%d].data len = %d, want %d", i, len(s.blocks[i].data), expected)
		}
	}

	// valid value
	s.setBlockSize(1024)
	for i := 0; i < s.blockNum; i++ {
		expected := 1024 + blockHeaderLength
		if len(s.blocks[i].data) != expected {
			t.Errorf("blocks[%d].data len = %d, want %d", i, len(s.blocks[i].data), expected)
		}
	}
}

func TestDealOpts_Blksize(t *testing.T) {
	s := &sender{}
	s.setBlockNum(2)
	s.setBlockSize(0)

	s.dealOpts(options{"blksize": "1024"})
	for i := 0; i < s.blockNum; i++ {
		expected := 1024 + blockHeaderLength
		if len(s.blocks[i].data) != expected {
			t.Errorf("after dealOpts, blocks[%d].data len = %d, want %d", i, len(s.blocks[i].data), expected)
		}
	}
}

func TestDealOpts_Nil(t *testing.T) {
	s := &sender{}
	s.setBlockNum(1)
	s.setBlockSize(0)
	// should not panic
	s.dealOpts(nil)
}
