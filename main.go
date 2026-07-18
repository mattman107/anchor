package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

func main() {
	server := NewServer()

	sigsCa := make(chan os.Signal, 1)
	signal.Notify(sigsCa, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigsCa
		signal.Stop(sigsCa)
		log.Println("Shutting down server...")
		buf := make([]byte, 1<<20)
		stacklen := runtime.Stack(buf, true)
		log.Printf("=== Goroutine Dump ===\n%s\n=== End ===", buf[:stacklen])

		server.shutdown()
	}()

	go processStdin(server)

	server.Start()
}
