package sftp

import (
	"os"
	"path/filepath"
	"testing"
)

func createTempFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile.dat")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewFiler(t *testing.T) {
	content := []byte("hello sftp test data")
	path := createTempFile(t, content)

	f, err := NewFiler(path)
	if err != nil {
		t.Fatalf("NewFiler error: %v", err)
	}
	if f.Filename != path {
		t.Errorf("Filename = %q, want %q", f.Filename, path)
	}
	if f.FileSize != int64(len(content)) {
		t.Errorf("FileSize = %d, want %d", f.FileSize, len(content))
	}
	if f.MD5 == "" {
		t.Error("MD5 is empty")
	}
	if f.State != stateSync {
		t.Errorf("State = %d, want %d", f.State, stateSync)
	}
}

func TestNewFiler_NotExist(t *testing.T) {
	_, err := NewFiler("/nonexistent/path/file.txt")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestIsSameFiler(t *testing.T) {
	a := Filer{Filename: "f.txt", FileSize: 100, MD5: "abc"}
	b := Filer{Filename: "f.txt", FileSize: 100, MD5: "abc"}
	if !isSameFiler(a, b) {
		t.Error("expected same")
	}
	b.MD5 = "xyz"
	if isSameFiler(a, b) {
		t.Error("expected not same")
	}
}

func TestIsHalfFiler(t *testing.T) {
	// create a 20-byte file
	content := []byte("01234567890123456789")
	path := createTempFile(t, content)

	local := Filer{Filename: path, FileSize: 20}

	// simulate a server that has the first 10 bytes
	halfContent := content[:10]
	halfPath := createTempFile(t, halfContent)
	remote, _ := NewFiler(halfPath)
	remote.Filename = path // must match local filename

	if !isHalfFiler(local, *remote) {
		t.Error("expected half filer match")
	}

	// mismatched prefix
	wrongContent := []byte("xxxxxxxxxx")
	wrongPath := createTempFile(t, wrongContent)
	wrongRemote, _ := NewFiler(wrongPath)
	wrongRemote.Filename = path

	if isHalfFiler(local, *wrongRemote) {
		t.Error("expected no match for wrong prefix")
	}
}

func TestCheckFileForWrite_NotExist(t *testing.T) {
	client := Filer{Filename: "/nonexistent/path/file.txt"}
	resp := checkFileForWrite(client)
	if resp.ACK != ackNExist {
		t.Errorf("ACK = %d, want %d (ackNExist)", resp.ACK, ackNExist)
	}
}

func TestCheckFileForWrite_Same(t *testing.T) {
	path := createTempFile(t, []byte("hello"))
	filer, err := NewFiler(path)
	if err != nil {
		t.Fatal(err)
	}
	resp := checkFileForWrite(*filer)
	if resp.ACK != ackSame {
		t.Errorf("ACK = %d, want %d (ackSame)", resp.ACK, ackSame)
	}
}

func TestCheckFileForWrite_NotSame(t *testing.T) {
	path := createTempFile(t, []byte("hello"))
	client := Filer{
		Filename: path,
		FileSize: 5,
		MD5:      "different_md5_value",
	}
	resp := checkFileForWrite(client)
	if resp.ACK != ackNSame {
		t.Errorf("ACK = %d, want %d (ackNSame)", resp.ACK, ackNSame)
	}
}

func TestCheckFileForWrite_IsDir(t *testing.T) {
	dir := t.TempDir()
	client := Filer{Filename: dir}
	resp := checkFileForWrite(client)
	if resp.ACK != ackDir {
		t.Errorf("ACK = %d, want %d (ackDir)", resp.ACK, ackDir)
	}
}

func TestCheckFileForRead_NotExist(t *testing.T) {
	client := Filer{Filename: "/nonexistent/path/file.txt"}
	resp := checkFileForRead(client)
	if resp.ACK != ackNExist {
		t.Errorf("ACK = %d, want %d (ackNExist)", resp.ACK, ackNExist)
	}
}

func TestCheckFileForRead_Exists(t *testing.T) {
	path := createTempFile(t, []byte("hello"))
	// client has no prior info about the file
	client := Filer{Filename: path}
	resp := checkFileForRead(client)
	if resp.ACK != ackSame {
		t.Errorf("ACK = %d, want %d (ackSame)", resp.ACK, ackSame)
	}
	if resp.FileSize != 5 {
		t.Errorf("FileSize = %d, want 5", resp.FileSize)
	}
	if resp.MD5 == "" {
		t.Error("MD5 is empty")
	}
}

func TestCheckFileForRead_NotSame(t *testing.T) {
	path := createTempFile(t, []byte("hello"))
	client := Filer{
		Filename: path,
		FileSize: 5,
		MD5:      "different_md5",
	}
	resp := checkFileForRead(client)
	if resp.ACK != ackNSame {
		t.Errorf("ACK = %d, want %d (ackNSame)", resp.ACK, ackNSame)
	}
}
