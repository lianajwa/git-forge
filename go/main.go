// Command forge: one-process GitHub-compatible git forge.
//
// Binds an ephemeral port, prints "FORGE_LISTEN <port>" on stdout (the
// launcher reads it), then serves git smart HTTP, REST v3, GraphQL and MCP
// from an in-memory store until killed. Standard library only.
package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
)

func main() {
	runtime.GOMAXPROCS(2)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("FORGE_LISTEN %d\n", port)
	srv := newServer()
	if err := srv.serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
