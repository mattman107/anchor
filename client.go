package main

import (
	"log"
	"net"
	"time"
)

const sendQueueSize = 256

// Client is a member of a room. Every field except id is owned by the room's
// goroutine: it is only touched from closures running on the room's event
// loop, which is why this file has no locks at all. The one exception is
// writeLoop, which runs per connection and communicates back by posting
// events to the room.
type Client struct {
	id   uint64
	room *Room

	conn         net.Conn
	sendCh       chan string // outgoing queue, drained by the connection's writeLoop
	team         *Team
	state        string // opaque client state blob, relayed in ALL_CLIENT_STATE
	saveLoaded   bool   // mirrored out of the blob for REQUEST_TEAM_STATE decisions
	online       bool
	lastActivity time.Time
}

// send enqueues a packet for the client's writer goroutine. It never blocks; a
// full queue means the client stopped draining its socket, and the session is
// torn down. Must run on the room goroutine — which also makes the queue-full
// disconnect a plain function call instead of a lock-ordering puzzle.
func (c *Client) send(packetType, packet string, quiet bool) {
	if c.sendCh == nil {
		return
	}

	if !c.room.server.quietMode.Load() && !quiet {
		log.Printf("Client %d <- Server: %s\n", c.id, packetType)
	}

	select {
	case c.sendCh <- packet:
		c.lastActivity = time.Now()
	default:
		log.Printf("Client %d send queue full, disconnecting\n", c.id)
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
func (c *Client) writeLoop(conn net.Conn, ch chan string) {
	defer conn.Close()
	defer logPanic("writeLoop")

	for packet := range ch {
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
