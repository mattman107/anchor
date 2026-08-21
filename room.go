package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/sjson"
)

const roomQueueSize = 1024
const MAX_TEAM_QUEUE = 512

// MAX_TEAM_QUEUE_BYTES bounds what a team's replay queue can actually cost.
// The packet count alone bounds nothing: 512 packets of MAX_PACKET_SIZE is 4 GB
// per team, and teams are minted per arbitrary id. The whole queue also has to
// fit inside one REQUEST_TEAM_STATE reply, and JSON-escaping it into that reply
// roughly doubles it, so this sits well under MAX_PACKET_SIZE.
const MAX_TEAM_QUEUE_BYTES = 4 * 1024 * 1024

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

	broadcasting    bool // a membership snapshot is being sent right now
	membershipDirty bool // a client dropped mid-snapshot; one more pass is owed
}

// Team state is plain data owned by the room goroutine.
type Team struct {
	id               string
	state            string   // last saved team state blob
	queue            []string // packets to replay on top of the saved state
	queueBytes       int      // total size of queue, kept in step with it
	requestingState  []uint64 // clients waiting for an UPDATE_TEAM_STATE
	droppedFromQueue int      // oldest queued packets discarded since the last full state
}

// enqueue appends a packet to the replay queue, dropping the oldest entries
// until the queue is within both its packet and byte bounds. Without a byte
// bound the queue grows to gigabytes while still looking "capped", and the
// REQUEST_TEAM_STATE reply built from it outgrows what a client will accept.
func (t *Team) enqueue(packet string) {
	t.queue = append(t.queue, packet)
	t.queueBytes += len(packet)

	// Never drop the packet just added, even if it alone is over the byte
	// bound — it is still capped by MAX_PACKET_SIZE on the way in
	drop := 0
	for drop < len(t.queue)-1 &&
		(len(t.queue)-drop > MAX_TEAM_QUEUE || t.queueBytes > MAX_TEAM_QUEUE_BYTES) {
		t.queueBytes -= len(t.queue[drop])
		drop++
	}
	if drop == 0 {
		return
	}

	kept := len(t.queue) - drop
	copy(t.queue, t.queue[drop:])
	for i := kept; i < len(t.queue); i++ {
		t.queue[i] = "" // release the dropped packets for collection
	}
	t.queue = t.queue[:kept]

	if t.droppedFromQueue == 0 {
		log.Printf("Team %s replay queue hit its bound (%d packets / %d bytes), dropping oldest entries",
			t.id, MAX_TEAM_QUEUE, MAX_TEAM_QUEUE_BYTES)
	}
	t.droppedFromQueue += drop
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

func containsClientId(ids []uint64, id uint64) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}

	return false
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

	requested := clientId
	duplicate := false

	c := r.clients[clientId]
	switch {
	case c == nil:

	case c.conn != nil && time.Since(c.lastPacket) < ACTIVE_CLIENT_WINDOW:
		// Somebody is playing under this id right now, so this handshake is not
		// them reconnecting — it is a second client configured with the same
		// id, which happens when a player edits it by hand. Taking the session
		// over would start a war: each client kicks the other, reconnects, and
		// kicks it back, broadcasting the whole room's membership every round.
		// Ids belong to the server, so hand this one a different one instead.
		clientId = r.server.mintClientId()
		duplicate = true
		c = nil

	case c.conn != nil:
		// Quiet incumbent: this is the reconnect case the takeover exists for.
		// Close now rather than letting the stale writer drain first. Draining
		// can take 10s per queued packet, and for all that time the displaced
		// connection's reader is still alive and still posting packets against
		// this client. Nothing queued for a session being displaced is worth
		// delivering anyway.
		log.Printf("Client %v reconnected, closing stale session\n", clientId)
		c.conn.Close()
		r.detach(c)
	}

	fields := parseClientStateFields(env.ClientState)
	state, _ := sjson.Set(string(env.ClientState), "clientId", clientId)

	if c == nil {
		c = &Client{id: clientId, room: r}
		r.clients[clientId] = c
	}

	c.conn = conn
	c.sendCh = make(chan string, sendQueueSize)
	c.queued = &atomic.Int64{}
	c.state = state
	c.team = r.findOrCreateTeam(fields.TeamID)
	c.saveLoaded = fields.IsSaveLoaded
	c.online = true
	c.lastActivity = time.Now()
	c.lastPacket = c.lastActivity // the handshake itself is inbound traffic
	go c.writeLoop(conn, c.sendCh, c.queued)

	r.server.setClientRoom(clientId, r)
	r.broadcastAllClientState()

	roomState, _ := sjson.SetRaw(`{"type":"UPDATE_ROOM_STATE"}`, "state", r.state)
	c.send(PacketUpdateRoomState, roomState, false)

	if duplicate {
		log.Printf("Client id %v is in use by an active connection in room %s; assigned %v instead\n",
			requested, r.id, clientId)
		c.sendServerMessage(fmt.Sprintf(
			"Client ID %d is already in use in this room, so you were given ID %d. Change your client ID in settings to keep it.",
			requested, clientId))
	}

	return c
}

// detach closes the client's writer, which flushes queued packets and then
// closes the socket.
func (r *Room) detach(c *Client) {
	if c.sendCh != nil {
		close(c.sendCh)
		c.sendCh = nil
		c.queued = nil // the old writeLoop keeps its own reference to drain
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
func (r *Room) handlePacket(c *Client, conn net.Conn, env *Envelope) {
	// A reconnect takes the client over, but the displaced connection's reader
	// is a separate goroutine that keeps reading until its socket closes. Until
	// then it is still posting packets for this client, and applying them would
	// let the old session write into the new one's state — a client's team,
	// position and save flags all get overwritten by whatever the displaced
	// connection sends. Same conn-identity check disconnect uses.
	if c.conn != conn {
		return
	}

	c.lastActivity = time.Now()
	c.lastPacket = c.lastActivity

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
			// UPDATE_TEAM_STATE reply can be forwarded back. Recorded once
			// however often it asks, so a client repeating the request can't
			// grow this without bound or earn itself duplicate replies.
			if !containsClientId(team.requestingState, c.id) {
				team.requestingState = append(team.requestingState, c.id)
			}
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

	// The reply has to fit through the client's own frame limit. Check that
	// from the raw sizes first: marshalling escapes every queued packet into a
	// JSON string, so building it just to measure it can allocate many times
	// MAX_PACKET_SIZE before finding out it does not fit.
	if len(team.state)+2*team.queueBytes+64 > MAX_PACKET_SIZE {
		log.Printf("Team %s state plus %d queued packets (%d raw bytes) cannot fit the %d byte limit; sending state only",
			team.id, len(reply.Queue), team.queueBytes, MAX_PACKET_SIZE)
		reply.Queue = []string{}
	}

	packet := marshalPacket(reply)
	if len(packet) > MAX_PACKET_SIZE {
		log.Printf("Team %s reply is %d bytes, over the %d byte limit; sending state only",
			team.id, len(packet), MAX_PACKET_SIZE)
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
	team.queueBytes = 0
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

// clientStateSnapshot holds the ALL_CLIENT_STATE array built once, plus where
// each member's own entry sits inside it, so a packet for any single member is
// three copies rather than a re-parse of the whole snapshot.
type clientStateSnapshot struct {
	states string
	starts []int
	ends   []int
}

func newClientStateSnapshot(states []string) *clientStateSnapshot {
	s := &clientStateSnapshot{
		starts: make([]int, len(states)),
		ends:   make([]int, len(states)),
	}

	var array strings.Builder
	for i, state := range states {
		if i > 0 {
			array.WriteByte(',')
		}
		s.starts[i] = array.Len()
		array.WriteString(state)
		s.ends[i] = array.Len()
	}
	s.states = array.String()

	return s
}

// packetFor returns the snapshot as member i sees it: every state, with only
// entry i marked self.
func (s *clientStateSnapshot) packetFor(i int) string {
	const head = `{"type":"` + PacketAllClientState + `","state":[`
	const tail = `]}`

	// Only this member's own entry is rewritten, never the whole snapshot
	self, _ := sjson.Set(s.states[s.starts[i]:s.ends[i]], "self", true)

	var packet strings.Builder
	packet.Grow(len(head) + s.starts[i] + len(self) + len(s.states) - s.ends[i] + len(tail))
	packet.WriteString(head)
	packet.WriteString(s.states[:s.starts[i]])
	packet.WriteString(self)
	packet.WriteString(s.states[s.ends[i]:])
	packet.WriteString(tail)

	return packet.String()
}

// broadcastAllClientState sends every member the same membership snapshot,
// with only their own entry marked self. Running on the room goroutine makes
// snapshot ordering automatic — two joins can't interleave their broadcasts.
//
// N packets each carrying N states is inherently O(N^2) bytes on the wire, but
// it need not be O(N^2) work to build. The obvious version — join the states
// once, then sjson.Set "state.<i>.self" per member — re-parses and re-copies
// the entire snapshot N times over. Splicing instead brings allocation down to
// the size of what is actually sent.
//
// This runs on every join and every disconnect, so it is the hot path whenever
// a client reconnect-loops in a busy room.
func (r *Room) broadcastAllClientState() {
	// Sending a snapshot can tear a client down (a full send queue is treated
	// as a dead session), and that teardown broadcasts again. Left alone those
	// nest one deep per dropped client, so a room whose clients all stall at
	// once — a network hiccup is enough — rebuilds the whole snapshot once per
	// client per level. Collapse that into at most one extra pass.
	if r.broadcasting {
		r.membershipDirty = true
		return
	}

	r.broadcasting = true
	defer func() {
		r.broadcasting = false
		r.membershipDirty = false
	}()

	r.sendClientStateSnapshot()
	if r.membershipDirty {
		r.membershipDirty = false
		r.sendClientStateSnapshot() // reflect whoever dropped during the first pass
	}
}

func (r *Room) sendClientStateSnapshot() {
	if len(r.clients) == 0 {
		return
	}

	members := make([]*Client, 0, len(r.clients))
	states := make([]string, 0, len(r.clients))
	for _, c := range r.clients {
		members = append(members, c)
		states = append(states, c.state)
	}

	snapshot := newClientStateSnapshot(states)
	for i, c := range members {
		// One packet alive at a time: holding all N at once would put N^2
		// bytes on the heap simultaneously
		c.send(PacketAllClientState, snapshot.packetFor(i), false)
	}
}

// pruneStaleClients forgets clients that went offline longer than
// CLIENT_RETENTION ago. Offline clients are kept so a reconnect resumes the
// same id, but a room that always has someone in it never expires, so without
// an upper bound its membership — and every snapshot built from it — grows
// with the number of players who have ever joined rather than who is present.
//
// A pruned client can still reconnect; it just arrives as a new member with
// the state from its handshake. Team save state lives on the Team, not here.
func (r *Room) pruneStaleClients() bool {
	pruned := 0
	for id, c := range r.clients {
		if c.conn == nil && time.Since(c.lastActivity) > CLIENT_RETENTION {
			delete(r.clients, id)
			pruned++
		}
	}

	if pruned > 0 {
		log.Printf("Room %s forgot %d client(s) offline for over %v", r.id, pruned, CLIENT_RETENTION)
	}

	return pruned > 0
}

// sweepIfInactive shuts the room down when nothing has happened in it for
// INACTIVITY_TIMEOUT. Connected clients are heartbeated every HEARTBEAT, which
// counts as activity, so only rooms with no live connections expire.
func (r *Room) sweepIfInactive() {
	if r.pruneStaleClients() {
		r.broadcastAllClientState()
	}

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
