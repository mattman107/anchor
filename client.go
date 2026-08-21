package main

import (
	"log"
	"net"
	"sync/atomic"
	"time"
)

// sendQueueSize is a backstop on queued packet count. The real bound is
// MAX_QUEUED_BYTES: 256 packets sounds conservative but says nothing about
// size, and at MAX_PACKET_SIZE it is 2 GB per connection. Bounding bytes
// instead lets this be generous, so a burst of small position updates has far
// more room than before rather than less.
const sendQueueSize = 2048

// MAX_QUEUED_BYTES is how much unsent data one connection may hold. Larger
// than MAX_PACKET_SIZE so a single legitimate max-size packet always fits.
const MAX_QUEUED_BYTES = 16 * 1024 * 1024

// Client is a member of a room. Every field except id is owned by the room's
// goroutine: it is only touched from closures running on the room's event
// loop, which is why this file has no locks at all. The one exception is
// writeLoop, which runs per connection and communicates back by posting
// events to the room.
type Client struct {
	id   uint64
	room *Room

	conn   net.Conn
	sendCh chan string // outgoing queue, drained by the connection's writeLoop
	// queued tracks bytes sitting in sendCh. Shared with this connection's
	// writeLoop, so it is atomic and replaced along with sendCh — a stale
	// writer draining an old channel must not decrement the new one's count.
	queued       *atomic.Int64
	team         *Team
	state        string // opaque client state blob, relayed in ALL_CLIENT_STATE
	saveLoaded   bool   // mirrored out of the blob for REQUEST_TEAM_STATE decisions
	online       bool
	lastActivity time.Time
}

// send enqueues a packet for the client's writer goroutine. It never blocks; a
// queue that is over budget means the client stopped draining its socket, and
// the session is torn down. Must run on the room goroutine — which also makes
// the queue-full disconnect a plain function call instead of a lock-ordering
// puzzle.
func (c *Client) send(packetType, packet string, quiet bool) {
	if c.sendCh == nil {
		return
	}

	if !c.room.server.quietMode.Load() && !quiet {
		log.Printf("Client %d <- Server: %s\n", c.id, packetType)
	}

	// An empty queue always accepts, so an oversized-but-legal packet is never
	// itself fatal
	queued := c.queued.Load()
	if queued > 0 && queued+int64(len(packet)) > MAX_QUEUED_BYTES {
		log.Printf("Client %d has %d MB queued unsent, disconnecting\n", c.id, queued/(1<<20))
		c.room.disconnect(c, c.conn)
		return
	}

	select {
	case c.sendCh <- packet:
		c.queued.Add(int64(len(packet)))
		c.lastActivity = time.Now()
	default:
		log.Printf("Client %d send queue full at %d packets, disconnecting\n", c.id, sendQueueSize)
		c.room.disconnect(c, c.conn)
	}
}

func (c *Client) sendEnvelope(env *Envelope) {
	c.send(env.Type, env.Raw, env.Quiet)
}

// sendServerMessage shows a message to the player in-game.
func (c *Client) sendServerMessage(message string) {
	if message == "" {
		message = "You have been disconnected by the server. Try to connect again in a bit!"
	}
	c.send(PacketServerMessage, marshalPacket(serverMessagePacket{Type: PacketServerMessage, Message: message}), false)
}

// disable tells the client to turn anchor off, then drops the connection.
// The writer flushes both queued packets before closing the socket.
func (c *Client) disable(message string) {
	c.sendServerMessage(message)
	c.send(PacketDisableAnchor, marshalPacket(disableAnchorPacket{Type: PacketDisableAnchor}), false)
	c.room.disconnect(c, c.conn)
}

// writeLoop is the sole writer for one connection, keeping packets in enqueue
// order. It owns closing the connection: it exits when the channel is closed
// (after flushing what was queued) or when a write fails.
func (c *Client) writeLoop(conn net.Conn, ch chan string, queued *atomic.Int64) {
	defer conn.Close()
	defer logPanic("writeLoop")

	for packet := range ch {
		// Released as soon as it leaves the queue, whether or not the write
		// succeeds — the bytes are no longer held here either way
		queued.Add(-int64(len(packet)))

		// A fresh deadline before every write, so a dead connection can't
		// stall the loop
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := writeFrame(conn, packet); err != nil {
			// Hand cleanup to the room goroutine; blocking here is fine,
			// this goroutine has nothing left to do
			c.room.post(func() { c.room.disconnect(c, conn) })
			return
		}
	}
}
