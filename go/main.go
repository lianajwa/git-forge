// main.go — the Go core's entrypoint: bind an ephemeral port on 127.0.0.1, print
// FORGE_LISTEN <port> for the Node launcher to read, then serve until killed.
// The bearer token comes from MAKEFUN_SHOP_TOKEN (empty accepts any non-empty token).

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	_ "net/http/pprof"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	forge := NewForge()
	server := newForgeServer(forge, token)
	// PORT pins the bind port for local testing (the run-suite harness uses 8091);
	// unset means an ephemeral port, which the trial env wants.
	port := 0
	if raw := os.Getenv("PORT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			port = n
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("[forge] listen: %v", err)
	}
	port = listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("FORGE_LISTEN %d\n", port)

	// FORGE_PPROF=1 starts a debug server on 127.0.0.1:6060 (pprof endpoints)
	// for local profiling only; unset in production, where it costs nothing.
	if os.Getenv("FORGE_PPROF") != "" {
		go func() {
			_ = http.ListenAndServe("127.0.0.1:6060", nil)
		}()
	}

	// Exit cleanly when the launcher kills us.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		os.Exit(0)
	}()

	// FORGE_NETHTTP=1 keeps the net/http server (A/B + fallback); the default is
	// the raw-TCP HTTP/1.1 server in rawhttp.go.
	if os.Getenv("FORGE_NETHTTP") != "" {
		if err := http.Serve(listener, server); err != nil {
			log.Fatalf("[forge] serve: %v", err)
		}
	} else {
		if err := serveRaw(listener, server); err != nil {
			log.Fatalf("[forge] serve: %v", err)
		}
	}
}
