package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/blurty/sftp"
)

// readHandler is called when client starts file download from server
func readHandler(filename string, rf io.ReaderFrom) error {
	file, err := os.Open(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	n, err := rf.ReadFrom(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	fmt.Printf("%d bytes sent\n", n)
	return nil
}

// writeHandler is called when client starts file upload to server
func writeHandler(filename string, wt io.WriterTo) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	n, err := wt.WriteTo(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return err
	}
	fmt.Printf("%d bytes received\n", n)
	return nil
}

func main() {
	addr := ":22345"
	if len(os.Args) >= 2 {
		addr = os.Args[1]
	}
	// use nil in place of handler to disable read or write operations
	s := sftp.NewServer(readHandler, writeHandler)
	s.SetTimeout(5 * time.Second)
	fmt.Printf("listening on %s\n", addr)
	err := s.ListenAndServe(addr) // blocks until s.Shutdown() is called
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}
