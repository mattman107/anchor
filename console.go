package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
)

const consoleHelp = `Available commands:
help: Show this help message
stats: Print server stats
quiet: Toggle quiet mode
roomCount: Show the number of rooms
clientCount: Show the number of clients
list: List all rooms and clients
stop <message>: Stop the server
message <clientId> <message>: Send a message to a client
messageAll <message>: Send a message to all clients
disable <clientId> <message>: Disable anchor on a client
disableAll <message>: Disable anchor on all clients
deleteRoom <roomID>: Disables anchor on all online clients in the room and deletes it
`

func getClientID(clientID string) uint64 {
	converted, err := strconv.ParseUint(clientID, 10, 64)
	if err != nil {
		log.Println("Given text was not a valid clientID.")
		return 0
	}

	return converted
}

// withOnlineClient looks up which room the client is in and runs fn on that
// room's goroutine, logging when the id is invalid or the client isn't online.
func (s *Server) withOnlineClient(idText string, fn func(*Client)) {
	clientId := getClientID(idText)
	if clientId == 0 {
		return
	}

	room := s.roomOfClient(clientId)
	if room == nil {
		log.Println("Client", clientId, "not found")
		return
	}

	room.post(func() {
		if client := room.clients[clientId]; client != nil && client.online {
			fn(client)
		}
	})
}

// forEachOnlineClient runs fn for every online client, on each client's own
// room goroutine.
func (s *Server) forEachOnlineClient(fn func(*Client)) {
	for _, room := range s.snapshotRooms() {
		r := room
		r.post(func() {
			for _, client := range r.clients {
				if client.online {
					fn(client)
				}
			}
		})
	}
}

func processStdin(s *Server) {
	reader := bufio.NewReader(os.Stdin)
	for {
		input, err := reader.ReadString('\n')

		if err != nil {
			if err == io.EOF {
				log.Println("Got an EOF from stdin. Closing console goroutine.")
				return
			}

			log.Println("Error reading from stdin:", err)
			continue
		}

		s.runConsoleCommand(strings.Split(strings.TrimRight(input, "\r\n"), " "))
	}
}

// requireArg reports whether args has a non-empty value at index i, logging
// what was missing when it doesn't. Every command that indexes past args[0]
// must go through this: an out-of-range index here is a panic on the console
// goroutine, and that takes the whole server down with it.
func requireArg(args []string, i int) bool {
	if i < len(args) && args[i] != "" {
		return true
	}

	log.Printf("%s is missing an argument. See: help", args[0])
	return false
}

// runConsoleCommand executes one console line. The recover is a backstop, not
// the plan — commands are expected to validate their own arguments — but a
// typo at the console must never be able to stop the server.
func (s *Server) runConsoleCommand(args []string) {
	defer logPanic("console command " + args[0])

	switch args[0] {
	case "roomCount":
		log.Println("Room count:", s.roomCount())
	case "clientCount":
		log.Println("Client count:", s.onlineCount())
	case "quiet":
		s.quietMode.Store(!s.quietMode.Load())
		log.Println("Quiet mode:", s.quietMode.Load())
	case "stats":
		log.Println("Games Complete:", s.gameCompleteCount.Load())
	case "list":
		for _, room := range s.snapshotRooms() {
			r := room
			r.post(func() {
				log.SetFlags(0)
				log.Println("Room", r.id+":")
				for _, client := range r.clients {
					log.Printf("  Client %d: %s", client.id, client.state)
				}
				log.SetFlags(log.LstdFlags)
			})
		}
	case "disable":
		if !requireArg(args, 1) {
			return
		}
		s.withOnlineClient(args[1], func(client *Client) {
			log.Println("[Server] DISABLE_ANCHOR packet ->", client.id)
			client.disable(strings.Join(args[2:], " "))
		})
	case "disableAll":
		log.Println("[Server] DISABLE_ANCHOR packet -> All")
		message := strings.Join(args[1:], " ")
		s.forEachOnlineClient(func(client *Client) {
			client.disable(message)
		})
	case "message":
		if !requireArg(args, 1) {
			return
		}
		s.withOnlineClient(args[1], func(client *Client) {
			log.Println("[Server] SERVER_MESSAGE packet ->", client.id)
			client.sendServerMessage(strings.Join(args[2:], " "))
		})
	case "messageAll":
		log.Println("[Server] SERVER_MESSAGE packet -> All")
		message := strings.Join(args[1:], " ")
		s.forEachOnlineClient(func(client *Client) {
			client.sendServerMessage(message)
		})
	case "deleteRoom":
		if !requireArg(args, 1) {
			return
		}

		room := s.lookupRoom(args[1])
		if room == nil {
			log.Println("Room", args[1], "not found")
			return
		}

		room.post(func() {
			for _, client := range room.clients {
				if client.online {
					client.disable("Deleting your room. Goodbye!")
				}
			}
			room.shutdown()
		})
	case "stop":
		s.forEachOnlineClient(func(client *Client) {
			client.sendServerMessage("Server restarting. Check back in a bit!")
		})

		s.shutdown()
	default:
		log.Print(consoleHelp)
	}
}
