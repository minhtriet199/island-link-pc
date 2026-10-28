// island-link-pc: the PC side of Island Link (talks to the island-link-android phone app).
//
//	island-link-pc serve    run the link (Island starts this at login)
//	island-link-pc pair     make a one-time pairing code and show it as a QR
//	island-link-pc status   print the link state as JSON
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0"

// port the phone connects to (TLS). Same Wi-Fi only for now.
const port = 47800

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "pair":
		err = pairCmd(len(os.Args) > 2 && os.Args[2] == "--uri")
	case "status":
		err = statusCmd()
	case "version", "--version":
		fmt.Println("island-link-pc", version)
	default:
		fmt.Fprintln(os.Stderr, "usage: island-link-pc [serve|pair [--uri]|status|version]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "island-link-pc:", err)
		os.Exit(1)
	}
}
