package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const INACTIVITY_TIMEOUT = 5 * time.Minute
const HEARTBEAT = 30 * time.Second
const MAX_PACKET_SIZE = 8 * 1024 * 1024
const INITIAL_SCAN_BUFFER = 64 * 1024

type Server struct {
	listener          net.Listener
	quietMode         atomic.Bool
	gameCompleteCount atomic.Uint64
	nextClientId      atomic.Uint64

	// mu guards only the two registries below. All game state lives inside
	// rooms, owned by their goroutines; nothing ever holds mu while waiting on
	// a room, so there is no lock ordering to get wrong.
	mu          sync.Mutex
	rooms       map[string]*Room
	clientRooms map[uint64]*Room // which room each *online* client is in
}

// Stats is the schema of stats.json, also read by the discord bot.
type Stats struct {
	GameCompleteCount  uint64 `json:"gameCompleteCount"`
	OnlineCount        int    `json:"onlineCount"`
	LastStatsHeartbeat int64  `json:"lastStatsHeartbeat"`
	UniqueCount        uint64 `json:"uniqueCount"`
	Pid                int    `json:"pid"`
}

func NewServer() *Server {
	s := &Server{
		rooms:       map[string]*Room{},
		clientRooms: map[uint64]*Room{},
	}

	s.quietMode.Store(true)

	return s
}

func (s *Server) Start() {
	listener, err := net.Listen("tcp", ":43383")
	if err != nil {
		log.Fatal(err)
	}
	s.listener = listener

	s.parseStats()

	go s.runPeriodic("cleanupInactiveRooms", s.cleanupInactiveRooms)
	go s.runPeriodic("heartbeat", s.heartbeat)
	go s.runPeriodic("statsHeartbeat", s.saveStats)

	log.Println("Server running on :43383")
	log.Println("Quiet mode:", s.quietMode.Load())

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Println("Error with listener:", err)
				break
			}
			log.Println("Error accepting connection:", err)
			continue
		}

		go s.handleConnection(conn)
	}
}

// logPanic recovers a panic and logs it. Used as `defer logPanic("name")` in
// goroutines that must not take the whole server down.
func logPanic(name string) {
	if r := recover(); r != nil {
		log.Printf("Panic in %s: %v", name, r)
	}
}

// runPeriodic runs fn every HEARTBEAT until the process exits.
func (s *Server) runPeriodic(name string, fn func()) {
	defer logPanic(name)

	ticker := time.NewTicker(HEARTBEAT)
	defer ticker.Stop()

	for range ticker.C {
		fn()
	}
}

// Registry helpers. These take mu briefly and never call into a room while
// holding it. Every one of them releases mu with defer: a panic anywhere under
// this lock would otherwise leave it held, and since panics on the room and
// console goroutines are recovered rather than fatal, the process would keep
// running with every registry operation deadlocked behind it.

func (s *Server) findOrCreateRoom(roomId string, ownerClientId uint64, roomState string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomId]
	if !ok {
		room = NewRoom(s, roomId, ownerClientId, roomState)
		s.rooms[roomId] = room
		go room.run()
	}

	return room
}

func (s *Server) removeRoom(roomId string, room *Room) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rooms[roomId] == room {
		delete(s.rooms, roomId)
	}
}

// lookupRoom returns the room registered under roomId, or nil.
func (s *Server) lookupRoom(roomId string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.rooms[roomId]
}

// roomOfClient returns the room an online client is in, or nil.
func (s *Server) roomOfClient(clientId uint64) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.clientRooms[clientId]
}

func (s *Server) snapshotRooms() []*Room {
	s.mu.Lock()
	defer s.mu.Unlock()

	rooms := make([]*Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room)
	}

	return rooms
}

func (s *Server) setClientRoom(clientId uint64, room *Room) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clientRooms[clientId] = room
}

func (s *Server) clearClientRoom(clientId uint64, room *Room) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.clientRooms[clientId] == room {
		delete(s.clientRooms, clientId)
	}
}

func (s *Server) onlineCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clientRooms)
}

func (s *Server) roomCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rooms)
}

func (s *Server) parseStats() {
	data, err := os.ReadFile("stats.json")
	if err != nil {
		log.Println("Error reading stats.json file:", err)
	}

	var stats Stats
	if err := json.Unmarshal(data, &stats); err != nil {
		log.Println("Error parsing stats.json:", err)
	}
	s.gameCompleteCount.Store(stats.GameCompleteCount)
	s.nextClientId.Store(stats.UniqueCount)

	// Save stats immediately to update lastStatsHeartbeat
	s.saveStats()
}

func (s *Server) saveStats() {
	data, _ := json.Marshal(Stats{
		GameCompleteCount:  s.gameCompleteCount.Load(),
		OnlineCount:        s.onlineCount(),
		LastStatsHeartbeat: time.Now().UnixMilli(),
		UniqueCount:        s.nextClientId.Load(),
		Pid:                os.Getpid(),
	})

	if err := os.WriteFile("./stats.json", data, 0644); err != nil {
		log.Println("Error writing json to file: ", err)
	}
}

// shutdown persists stats, stops accepting connections, and exits.
func (s *Server) shutdown() {
	s.saveStats()
	s.listener.Close()
	os.Exit(0)
}

func (s *Server) cleanupInactiveRooms() {
	for _, room := range s.snapshotRooms() {
		room.post(room.sweepIfInactive)
	}
}

func (s *Server) heartbeat() {
	log.Println("Clients Online & Threads Running", s.onlineCount(), runtime.NumGoroutine())

	for _, room := range s.snapshotRooms() {
		room.post(room.heartbeatIdleClients)
	}
}

// handleConnection is the reader side of one connection: it parses each frame
// into an Envelope once, then posts the work to the client's room. It touches
// no shared state itself.
func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	defer logPanic("handleConnection")

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, INITIAL_SCAN_BUFFER), MAX_PACKET_SIZE)
	scanner.Split(splitNullByte)

	var client *Client
	var room *Room

	for scanner.Scan() {
		env, err := parseEnvelope(scanner.Text())
		if err != nil {
			log.Printf("Invalid JSON packet: %s\n", scanner.Text())
			continue
		}
		if env.Type == "" {
			log.Println("Packet missing type")
			continue
		}

		// Health check, answered without a handshake
		if env.Type == PacketStats {
			s.replyStats(client, room, conn)
			continue
		}

		if client == nil {
			if env.Type != PacketHandshake {
				log.Println("Client must handshake first")
				continue
			}

			client, room = s.joinRoom(env, conn)
			log.Printf("Client %v Connected\n", client.id)
			continue
		}

		room.post(func() { room.handlePacket(client, env) })
	}

	if client != nil {
		// Single teardown path: the room decides whether this conn is still
		// the client's current session and broadcasts if it was
		room.post(func() { room.disconnect(client, conn) })

		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				log.Printf("Client %v sent a packet over the %d byte limit, disconnecting", client.id, MAX_PACKET_SIZE)
			} else {
				log.Printf("Client %v disconnected with error: %v", client.id, err)
			}
		} else {
			log.Printf("Client %v disconnected\n", client.id)
		}
	} else {
		log.Println("Unknown client disconnected.")
	}
}

// joinRoom resolves the client id, then asks the room to bind the connection.
//
// A room can shut down at any point in here — between the lookup and the post,
// or after the post while the join is still queued. post() reporting success
// only means the closure was enqueued, and a queued closure is discarded when
// the room's event loop exits, so waiting on the reply alone would block this
// connection forever. Waiting on r.done as well turns every one of those cases
// into another lap, which finds or creates a live room.
func (s *Server) joinRoom(env *Envelope, conn net.Conn) (*Client, *Room) {
	clientId := s.resolveClientId(env.ClientID, env.RoomID)

	for {
		room := s.findOrCreateRoom(env.RoomID, clientId, string(env.RoomState))
		reply := make(chan *Client, 1)
		if !room.post(func() { reply <- room.join(clientId, env, conn) }) {
			continue
		}

		select {
		case client := <-reply:
			// nil means the join ran but the room had already closed
			if client != nil {
				return client, room
			}
		case <-room.done:
			// the join never ran, or ran and declined
		}
	}
}

// resolveClientId decides which id a handshake gets. An id that is currently
// online in the same room is the same player reconnecting — the room will take
// over the stale session. An id online in a different room isn't theirs, so a
// fresh one is minted (ids are globally unique per server).
func (s *Server) resolveClientId(requested uint64, roomId string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if requested != 0 {
		room, online := s.clientRooms[requested]
		if !online || room.id == roomId {
			return requested
		}
	}

	for {
		id := s.nextClientId.Add(1)
		if _, taken := s.clientRooms[id]; !taken {
			return id
		}
	}
}

// replyStats answers a STATS health check.
//
// Before the handshake this reader goroutine is the connection's only writer,
// so it writes the reply itself. Afterwards the client's writeLoop owns the
// socket, and writing here too would interleave these bytes with a packet
// writeLoop is midway through sending — the client then reads a truncated
// frame and drops the connection. So once there is a client, the reply goes
// through the same send queue as everything else.
func (s *Server) replyStats(client *Client, room *Room, conn net.Conn) {
	packet := marshalPacket(statsReplyPacket{
		Type:              PacketStats,
		UniqueCount:       s.nextClientId.Load(),
		GameCompleteCount: s.gameCompleteCount.Load(),
		OnlineCount:       s.onlineCount(),
	})

	if client == nil {
		writeFrame(conn, packet)
		return
	}

	room.post(func() { client.send(PacketStats, packet, true) })
}

// writeFrame writes one null-terminated packet to conn. It is the mirror of
// splitNullByte on the read side. Only ever called from the goroutine that
// owns the connection: the writeLoop, or the reader before a handshake.
func writeFrame(conn net.Conn, packet string) error {
	buf := make([]byte, len(packet)+1)
	copy(buf, packet)
	_, err := conn.Write(buf)
	return err
}

func splitNullByte(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
