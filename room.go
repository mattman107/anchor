package main

import (
	"encoding/json"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/sjson"
)

const roomQueueSize = 1024
const MAX_TEAM_QUEUE = 512

// Room is an actor: one goroutine (run) owns all room, team, and client state,
// and everything that touches that state executes as a closure posted to the
// events channel. There are no mutexes because there is no sharing —
// connection readers, the console, and the server's tickers post work here
// instead of reaching into the state themselves. Rooms are independent, so
// this serializes nothing across rooms.
type Room struct {
	id     string
	server *Server
	events chan func()
	done   chan struct{}

	// Owned by the room goroutine:
	state   string // room settings blob (opaque, relayed)
	clients map[uint64]*Client
	teams   map[string]*Team
	created time.Time // when the room was registered, for the inactivity sweep
	closed  bool      // shutdown has run; joins must be declined, not honored
}

// Team state is plain data owned by the room goroutine.
type Team struct {
	id               string
	state            string   // last saved team state blob
	queue            []string // packets to replay on top of the saved state
	requestingState  []uint64 // clients waiting for an UPDATE_TEAM_STATE
	droppedFromQueue int      // oldest queued packets discarded since the last full state
}

// enqueue appends a packet to the replay queue, dropping the oldest entries
// once the queue is full. Without the bound a team's queue grows until the
// room is swept, and the REQUEST_TEAM_STATE reply built from it outgrows what
// a client will accept.
func (t *Team) enqueue(packet string) {
	t.queue = append(t.queue, packet)
	if len(t.queue) <= MAX_TEAM_QUEUE {
		return
	}

	dropped := len(t.queue) - MAX_TEAM_QUEUE
	copy(t.queue, t.queue[dropped:])
	for i := MAX_TEAM_QUEUE; i < len(t.queue); i++ {
		t.queue[i] = ""
	}
	t.queue = t.queue[:MAX_TEAM_QUEUE]

	if t.droppedFromQueue == 0 {
		log.Printf("Team %s queue hit %d packets, dropping oldest entries", t.id, MAX_TEAM_QUEUE)
	}
	t.droppedFromQueue += dropped
}

func NewRoom(server *Server, id string, ownerClientId uint64, roomState string) *Room {
	state, _ := sjson.Set(roomState, "ownerClientId", ownerClientId)

	return &Room{
		id:      id,
		server:  server,
		events:  make(chan func(), roomQueueSize),
		done:    make(chan struct{}),
		state:   state,
		clients: map[uint64]*Client{},
		teams:   map[string]*Team{},
		created: time.Now(),
	}
}

// run executes posted events one at a time until the room shuts down.
func (r *Room) run() {
	for {
		select {
		case fn := <-r.events:
			r.dispatch(fn)
		case <-r.done:
			return
		}
	}
}

// dispatch isolates each event's panic so one bad packet can't kill the room.
func (r *Room) dispatch(fn func()) {
	defer logPanic("room " + r.id)
	fn()
}

// post schedules fn on the room goroutine. A false return means the room was
// already shut down. A true return only means fn was ENQUEUED: if the room
// shuts down first, run() exits and the queued fn is discarded. Callers that
// wait for a result must therefore also watch r.done — see Server.joinRoom.
func (r *Room) post(fn func()) bool {
	select {
	case r.events <- fn:
		return true
	case <-r.done:
		return false
	}
}

func (r *Room) findOrCreateTeam(teamId string) *Team {
	team, ok := r.teams[teamId]
	if !ok {
		team = &Team{id: teamId, state: "{}"}
		r.teams[teamId] = team
	}

	return team
}

// join creates or resumes the client with this id and binds the new
// connection. A reconnect is a takeover: the stale session's writer is closed
// and the new connection replaces it.
func (r *Room) join(clientId uint64, env *Envelope, conn net.Conn) *Client {
	// The room can be shut down between this event being queued and run().
	// Binding the connection here would strand it in a room nothing can reach,
	// so decline and let the caller retry against a live room.
	if r.closed {
		return nil
	}

	fields := parseClientStateFields(env.ClientState)
	state, _ := sjson.Set(string(env.ClientState), "clientId", clientId)

	c := r.clients[clientId]
	if c == nil {
		c = &Client{id: clientId, room: r}
		r.clients[clientId] = c
	} else if c.conn != nil {
		log.Printf("Client %v reconnected, closing stale session\n", clientId)
		r.detach(c)
	}

	c.conn = conn
	c.sendCh = make(chan string, sendQueueSize)
	c.state = state
	c.team = r.findOrCreateTeam(fields.TeamID)
	c.saveLoaded = fields.IsSaveLoaded
	c.online = true
	c.lastActivity = time.Now()
	go c.writeLoop(conn, c.sendCh)

	r.server.setClientRoom(clientId, r)
	r.broadcastAllClientState()

	roomState, _ := sjson.SetRaw(`{"type":"UPDATE_ROOM_STATE"}`, "state", r.state)
	c.send(PacketUpdateRoomState, roomState, false)

	return c
}

// detach closes the client's writer, which flushes queued packets and then
// closes the socket.
func (r *Room) detach(c *Client) {
	if c.sendCh != nil {
		close(c.sendCh)
		c.sendCh = nil
	}
	c.conn = nil
}

// disconnect is the single teardown path for every way a session can end:
// reader EOF, write error, full send queue, takeover cleanup, admin kick.
// It is a no-op if conn is no longer the client's current connection, so a
// stale session's cleanup can't touch a reconnected client. The room always
// broadcasts the change, whatever the cause.
func (r *Room) disconnect(c *Client, conn net.Conn) {
	if conn == nil || c.conn != conn {
		return
	}

	r.detach(c)
	c.online = false
	c.saveLoaded = false
	c.state, _ = sjson.Set(c.state, "online", false)
	c.state, _ = sjson.Set(c.state, "isSaveLoaded", false)
	r.server.clearClientRoom(c.id, r)
	r.broadcastAllClientState()
}

// handlePacket applies server-side effects, then routes: a target client wins,
// then packet types the server interprets, then team-addressed packets, then
// a room broadcast.
func (r *Room) handlePacket(c *Client, env *Envelope) {
	c.lastActivity = time.Now()

	if !r.server.quietMode.Load() && !env.Quiet {
		log.Printf("Client %d -> Server: %s\n", c.id, env.Type)
	}

	// Server-side effects, applied before the packet is routed
	switch env.Type {
	case PacketUpdateClientState:
		fields := parseClientStateFields(env.State)
		c.team = r.findOrCreateTeam(fields.TeamID)
		c.saveLoaded = fields.IsSaveLoaded
		c.state, _ = sjson.Set(string(env.State), "clientId", c.id)
	case PacketGameComplete:
		r.server.gameCompleteCount.Add(1)
	}

	// A packet addressed to a specific client goes there and nowhere else
	if env.TargetClientID != nil {
		if target := r.clients[*env.TargetClientID]; target != nil {
			target.sendEnvelope(env)
		}
		return
	}

	switch env.Type {
	case PacketRequestTeamState:
		r.handleRequestTeamState(c, env)
	case PacketUpdateTeamState:
		r.handleUpdateTeamState(env)
	case PacketUpdateRoomState:
		r.state = string(env.State)
		r.broadcast(c, env)
	default:
		if env.TargetTeamID != nil {
			team := r.findOrCreateTeam(*env.TargetTeamID)
			if env.AddToQueue {
				team.enqueue(env.Raw)
			}
			r.broadcastTeam(c, team, env)
		} else {
			r.broadcast(c, env)
		}
	}
}

func (r *Room) handleRequestTeamState(c *Client, env *Envelope) {
	if env.TargetTeamID == nil {
		return
	}
	team := r.findOrCreateTeam(*env.TargetTeamID)

	for _, other := range r.clients {
		if other != c && other.conn != nil && other.team == team && other.saveLoaded {
			// A live teammate can answer; remember who asked so their
			// UPDATE_TEAM_STATE reply can be forwarded back
			team.requestingState = append(team.requestingState, c.id)
			r.broadcastTeam(c, team, env)
			return
		}
	}

	// Nobody is online with a loaded save: serve the state the server kept
	reply := teamStateReplyPacket{Type: PacketUpdateTeamState, Queue: team.queue}
	if reply.Queue == nil {
		reply.Queue = []string{}
	}
	if team.state != "{}" && team.state != "" {
		reply.State = json.RawMessage(team.state)
	}

	// The reply has to fit through the client's own frame limit, so drop the
	// replay queue rather than send something the client will reject
	packet := marshalPacket(reply)
	if len(packet) > MAX_PACKET_SIZE {
		log.Printf("Team %s state plus %d queued packets is %d bytes, over the %d byte limit; sending state only",
			team.id, len(reply.Queue), len(packet), MAX_PACKET_SIZE)
		reply.Queue = []string{}
		packet = marshalPacket(reply)
	}

	c.send(PacketUpdateTeamState, packet, false)
}

func (r *Room) handleUpdateTeamState(env *Envelope) {
	if env.TargetTeamID == nil {
		return
	}
	team := r.findOrCreateTeam(*env.TargetTeamID)

	requesting := team.requestingState
	team.state = string(env.State)
	team.queue = nil
	team.requestingState = nil
	team.droppedFromQueue = 0

	for _, id := range requesting {
		if target := r.clients[id]; target != nil {
			target.sendEnvelope(env)
		}
	}
}

// broadcast relays a packet to everyone in the room except its sender. The
// sender is identified by connection, not by the clientId written in the
// packet, so a client can't speak as somebody else.
func (r *Room) broadcast(from *Client, env *Envelope) {
	for _, c := range r.clients {
		if c != from {
			c.sendEnvelope(env)
		}
	}
}

func (r *Room) broadcastTeam(from *Client, team *Team, env *Envelope) {
	for _, c := range r.clients {
		if c != from && c.team == team {
			c.sendEnvelope(env)
		}
	}
}

// broadcastAllClientState sends every member the same membership snapshot,
// with only their own entry marked self. Running on the room goroutine makes
// snapshot ordering automatic — two joins can't interleave their broadcasts.
func (r *Room) broadcastAllClientState() {
	members := make([]*Client, 0, len(r.clients))
	states := make([]string, 0, len(r.clients))
	for _, c := range r.clients {
		members = append(members, c)
		states = append(states, c.state)
	}

	packet := `{"type":"` + PacketAllClientState + `","state":[` + strings.Join(states, ",") + `]}`
	for i, c := range members {
		withSelf, _ := sjson.Set(packet, "state."+strconv.Itoa(i)+".self", true)
		c.send(PacketAllClientState, withSelf, false)
	}
}

// sweepIfInactive shuts the room down when nothing has happened in it for
// INACTIVITY_TIMEOUT. Connected clients are heartbeated every HEARTBEAT, which
// counts as activity, so only rooms with no live connections expire.
func (r *Room) sweepIfInactive() {
	// Seeded with creation time so a room that exists but has not finished its
	// first join yet isn't swept the instant it appears
	last := r.created
	for _, c := range r.clients {
		if c.lastActivity.After(last) {
			last = c.lastActivity
		}
	}

	if time.Since(last) > INACTIVITY_TIMEOUT {
		log.Println("Room", r.id, "has been inactive for too long, deleting it")
		r.shutdown()
	}
}

// heartbeatIdleClients pings clients that haven't seen traffic lately so dead
// connections surface as write errors.
func (r *Room) heartbeatIdleClients() {
	for _, c := range r.clients {
		if c.conn != nil && time.Since(c.lastActivity) > HEARTBEAT {
			c.send(PacketHeartbeat, heartbeatPacket, true)
		}
	}
}

// shutdown detaches everyone still connected, unregisters the room, and stops
// the event loop. Runs on the room goroutine; posts after this return false.
func (r *Room) shutdown() {
	if r.closed {
		return
	}
	r.closed = true

	for _, c := range r.clients {
		if c.conn != nil {
			r.server.clearClientRoom(c.id, r)
			r.detach(c)
			c.online = false
		}
	}
	r.server.removeRoom(r.id, r)
	close(r.done)
}
