package main

import (
	"fmt"
	"log"
	"os"

	"github.com/blurty/sftp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <filepath> [server_addr]\n", os.Args[0])
		os.Exit(1)
	}
	path := os.Args[1]
	addr := "127.0.0.1:22345"
	if len(os.Args) >= 3 {
		addr = os.Args[2]
	}
	client, err := sftp.NewClient(addr)
	if err != nil {
		log.Fatal(err)
	}
	err = client.SendFile(path)
	if err != nil {
		fmt.Printf("send file %s failed: %v\n", path, err)
	} else {
		fmt.Printf("send file %s succeeded\n", path)
	}
}
