package main

import (
	"fmt"
	"log"

	"github.com/blurty/sftp"
)

func main() {
	path := "./test.txt"
	client, err := sftp.NewClient("127.0.0.1:22345")
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
