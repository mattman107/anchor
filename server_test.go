package main

// Server-level tests: the packet/queue limits carried over from main, and the
// room lifecycle edges around joins racing shutdowns.
//	go test -v -run TestRB

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const addr = "127.0.0.1:43383"

var testServer *Server

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "anchor-rb")
	os.Chdir(dir)
	log.SetOutput(os.Stderr)
	testServer = NewServer()
	go testServer.Start()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Exit(m.Run())
}

type peer struct {
	conn net.Conn
	sc   *bufio.Scanner
}

func dial(t *testing.T) *peer {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	sc.Split(splitNullByte)
	return &peer{conn: c, sc: sc}
}

func (p *peer) send(f string, a ...interface{}) {
	fmt.Fprintf(p.conn, f, a...)
	p.conn.Write([]byte{0})
}

func (p *peer) handshake(room string, id uint64) {
	p.send(`{"type":"HANDSHAKE","roomId":%q,"clientId":%d,"roomState":{},"clientState":{"teamId":"1","isSaveLoaded":true}}`, room, id)
}

// recv reads one frame, or returns "" on timeout/EOF.
func (p *peer) recv(d time.Duration) string {
	p.conn.SetReadDeadline(time.Now().Add(d))
	if !p.sc.Scan() {
		return ""
	}
	return p.sc.Text()
}

// drain reads until the connection goes quiet for d. It ends on a read
// timeout, which puts the bufio.Scanner in its terminal error state — this
// peer cannot recv again afterwards. Use it to discard traffic you are done
// with, never as a "skip the join packets" step before more reading.
func (p *peer) drain(d time.Duration) {
	for p.recv(d) != "" {
	}
}

func onRoom(r *Room, fn func()) {
	done := make(chan struct{})
	if !r.post(func() { fn(); close(done) }) {
		return
	}
	<-done
}

func room(t *testing.T, id string) *Room {
	t.Helper()
	testServer.mu.Lock()
	defer testServer.mu.Unlock()
	if r := testServer.rooms[id]; r != nil {
		return r
	}
	t.Fatalf("room %q not found", id)
	return nil
}

// Bug A: packets up to MAX_PACKET_SIZE must survive again (was 64 KB).
func TestRBPacketSizeCeiling(t *testing.T) {
	for _, size := range []int{32 * 1024, 100 * 1024, 1024 * 1024, 4 * 1024 * 1024} {
		p := dial(t)
		p.handshake("size-room", 0)
		p.drain(300 * time.Millisecond)
		p.send(`{"type":"UPDATE_CLIENT_STATE","state":{"teamId":"1","pad":%q}}`, strings.Repeat("x", size))
		time.Sleep(400 * time.Millisecond)

		p.conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err := p.conn.Read(make([]byte, 1))
		alive := err == nil || strings.Contains(fmt.Sprint(err), "timeout")
		t.Logf("%5d KB packet: connection alive = %v", size/1024, alive)
		if !alive {
			t.Errorf("%d KB rejected — under MAX_PACKET_SIZE (%d)", size, MAX_PACKET_SIZE)
		}
		p.close()
	}
}

func (p *peer) close() { p.conn.Close() }

// Bug C: the team replay queue is bounded again.
func TestRBTeamQueueBounded(t *testing.T) {
	const id = "queue-room"
	p := dial(t)
	defer p.close()
	p.handshake(id, 0)
	go p.drain(30 * time.Second)

	pad := strings.Repeat("y", 32*1024)
	for i := 0; i < 5000; i++ {
		p.send(`{"type":"JUNK","targetTeamId":"t","addToQueue":true,"pad":%q}`, pad)
	}
	time.Sleep(3 * time.Second)

	r := room(t, id)
	var n, dropped int
	onRoom(r, func() {
		for _, tm := range r.teams {
			n += len(tm.queue)
			dropped += tm.droppedFromQueue
		}
	})
	t.Logf("5000 packets sent -> %d retained, %d dropped (cap %d)", n, dropped, MAX_TEAM_QUEUE)
	if n > MAX_TEAM_QUEUE {
		t.Errorf("queue exceeded MAX_TEAM_QUEUE: %d", n)
	}
	if dropped == 0 {
		t.Errorf("expected oldest entries to be dropped")
	}
}

// Quiet mode defaults on again, so production isn't logging every packet.
func TestRBQuietModeDefault(t *testing.T) {
	if !NewServer().quietMode.Load() {
		t.Error("quiet mode should default to true")
	}
}

// Regression guard: the actor model's fix for the writeLoop leak survived.
func TestRBNoReconnectLeak(t *testing.T) {
	const id = "leak-room"
	for i := 0; i < 30; i++ {
		p := dial(t)
		p.handshake(id, uint64(700000+i))
		p.drain(100 * time.Millisecond)
		p.close()
	}
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < 30; i++ {
		start := make(chan struct{})
		conns := []*peer{dial(t), dial(t)}
		var wg sync.WaitGroup
		for _, c := range conns {
			c := c
			wg.Add(1)
			go func() { defer wg.Done(); <-start; c.handshake(id, uint64(700000+i)) }()
		}
		close(start)
		wg.Wait()
		time.Sleep(30 * time.Millisecond)
		for _, c := range conns {
			c.close()
		}
	}
	time.Sleep(2 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	stuck := stacks("(*Client).writeLoop")
	t.Logf("goroutines %d -> %d, parked writeLoops: %d", before, runtime.NumGoroutine(), stuck)
	if stuck > 0 {
		t.Errorf("writeLoop leak is back")
	}
}

// Bug B: a join queued behind a room shutdown must not strand the connection.
// post() reporting success only means "enqueued"; the event loop discards
// queued events when it exits, so joinRoom has to watch room.done too and take
// another lap against a live room.
func TestRBJoinRoomSurvivesShutdown(t *testing.T) {
	const id = "shutdown-room"
	seed := dial(t)
	defer seed.close()
	seed.handshake(id, 0)
	seed.drain(300 * time.Millisecond)

	r := room(t, id)
	r.post(func() { time.Sleep(400 * time.Millisecond); r.shutdown() })
	time.Sleep(50 * time.Millisecond)

	const n = 20
	joined := make(chan bool, n)
	var victims []*peer
	defer func() {
		for _, v := range victims {
			v.close()
		}
	}()
	for i := 0; i < n; i++ {
		v := dial(t)
		victims = append(victims, v)
		go func() {
			v.handshake(id, 0)
			// ALL_CLIENT_STATE / UPDATE_ROOM_STATE only arrive once join ran
			v.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			joined <- v.sc.Scan()
		}()
	}

	ok := 0
	for i := 0; i < n; i++ {
		select {
		case got := <-joined:
			if got {
				ok++
			}
		case <-time.After(8 * time.Second):
		}
	}

	stuck := stacks("(*Server).joinRoom")
	t.Logf("%d/%d handshakes completed, %d parked in joinRoom", ok, n, stuck)
	if stuck > 0 {
		t.Errorf("%d handshakes still blocked on <-reply", stuck)
	}
	if ok != n {
		t.Errorf("only %d/%d clients joined; retry did not land them in a live room", ok, n)
	}
}

// The room the retries land in must be a live, registered one.
func TestRBRetryLandsInLiveRoom(t *testing.T) {
	const id = "retry-room"
	seed := dial(t)
	defer seed.close()
	seed.handshake(id, 0)
	seed.drain(300 * time.Millisecond)

	old := room(t, id)
	onRoom(old, old.shutdown)
	time.Sleep(100 * time.Millisecond)

	p := dial(t)
	defer p.close()
	p.handshake(id, 0)
	p.drain(500 * time.Millisecond)

	fresh := room(t, id)
	if fresh == old {
		t.Fatal("still registered to the shut-down room")
	}
	var n int
	var closed bool
	onRoom(fresh, func() { n = len(fresh.clients); closed = fresh.closed })
	t.Logf("fresh room has %d client(s), closed=%v", n, closed)
	if n != 1 || closed {
		t.Errorf("expected one client in a live room, got %d clients closed=%v", n, closed)
	}
}

// A brand-new room must not be swept before its first join completes.
func TestRBFreshRoomNotSweptInstantly(t *testing.T) {
	r := NewRoom(testServer, "sweep-probe", 1, "{}")
	go r.run()
	defer func() { onRoom(r, r.shutdown) }()

	onRoom(r, r.sweepIfInactive)

	var closed bool
	onRoom(r, func() { closed = r.closed })
	if closed {
		t.Error("empty brand-new room was swept immediately")
	}
	t.Log("empty room measured from creation time, not the zero time")
}

func stacks(needle string) int {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, needle) && strings.Contains(g, "chan receive") {
			count++
		}
	}
	return count
}

// Hammer: joins racing shutdowns on the same room id, repeatedly. Nothing may
// hang, leak a goroutine, or trip the race detector.
func TestRBJoinShutdownStress(t *testing.T) {
	const id = "stress-room"
	seed := dial(t)
	seed.handshake(id, 0)
	seed.drain(300 * time.Millisecond)
	seed.close()

	runtime.GC()
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var joined, failed int64
	for round := 0; round < 25; round++ {
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := dial(t)
				defer p.close()
				p.handshake(id, 0)
				p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if p.sc.Scan() {
					atomic.AddInt64(&joined, 1)
				} else {
					atomic.AddInt64(&failed, 1)
				}
			}()
		}
		testServer.mu.Lock()
		r := testServer.rooms[id]
		testServer.mu.Unlock()
		if r != nil {
			r.post(r.shutdown)
		}
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()

	time.Sleep(2 * time.Second)
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	stuckJoin := stacks("(*Server).joinRoom")
	stuckWrite := stacks("(*Client).writeLoop")
	t.Logf("joined=%d failed=%d | goroutines %d -> %d | parked joinRoom=%d writeLoop=%d",
		joined, failed, before, runtime.NumGoroutine(), stuckJoin, stuckWrite)

	if stuckJoin > 0 {
		t.Errorf("%d handshakes parked in joinRoom", stuckJoin)
	}
	if stuckWrite > 0 {
		t.Errorf("%d writeLoops parked on an orphaned channel", stuckWrite)
	}
}

// Finding 4: only one goroutine may ever write to a connection.
//
// writeLoop owns the socket once a client exists. handleConnection used to
// answer STATS with its own conn.Write from the reader goroutine, so a STATS
// reply could land in the middle of a packet writeLoop was still sending and
// truncate the client's frame. This asserts the routing decision directly:
// before a handshake the reader writes, after one the reply is queued.
func TestSrvStatsNeverWritesToALiveConn(t *testing.T) {
	// Pre-handshake: the reader goroutine is the only writer, so it replies
	// on the connection itself.
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	go testServer.replyStats(nil, nil, srv)

	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("pre-handshake STATS was not written to the conn: %v", err)
	}
	if !strings.Contains(string(buf[:n]), `"type":"STATS"`) {
		t.Fatalf("unexpected pre-handshake reply: %q", buf[:n])
	}
	t.Log("no client yet -> written directly, as the connection's only writer")

	// Post-handshake: writeLoop owns the socket. The reply must go through the
	// send queue and must not touch the conn. net.Pipe is unbuffered, so any
	// stray write would surface as bytes readable here.
	srv2, cli2 := net.Pipe()
	defer srv2.Close()
	defer cli2.Close()

	r := NewRoom(testServer, "stats-unit-room", 1, "{}")
	go r.run()
	defer func() { onRoom(r, r.shutdown) }()

	c := &Client{id: 1, room: r, conn: srv2, sendCh: make(chan string, 4), online: true}
	onRoom(r, func() { r.clients[1] = c })

	// Run it off-goroutine: net.Pipe is synchronous, so a stray direct write
	// would block here rather than fail, and the suite would just time out.
	done := make(chan struct{})
	go func() { testServer.replyStats(c, r, srv2); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replyStats blocked writing to a live conn instead of queueing")
	}
	onRoom(r, func() {}) // flush the posted event

	cli2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if n, err := cli2.Read(buf); err == nil {
		t.Errorf("STATS bypassed the send queue and wrote %d bytes straight to a live conn", n)
	}

	select {
	case packet := <-c.sendCh:
		if !strings.Contains(packet, `"type":"STATS"`) {
			t.Fatalf("queued packet is not a STATS reply: %q", packet)
		}
		t.Log("client exists -> queued for writeLoop, conn untouched")
	default:
		t.Error("STATS reply never reached the client's send queue")
	}
}

// End to end: a handshaked client still gets STATS answers, in-band with
// everything else on its connection, and every frame stays valid.
func TestSrvStatsAfterHandshakeStillAnswered(t *testing.T) {
	const id = "stats-room"

	p := dial(t)
	defer p.close()
	p.handshake(id, 0)

	for i := 0; i < 20; i++ {
		p.send(`{"type":"STATS"}`)
	}

	// No drain first: it would end on a timeout and kill the scanner. The join
	// packets simply arrive ahead of the replies and are counted as frames.
	stats, corrupt := 0, 0
	for stats < 20 {
		f := p.recv(3 * time.Second)
		if f == "" {
			break
		}
		if !json.Valid([]byte(f)) {
			corrupt++
			continue
		}
		if strings.Contains(f, `"type":"STATS"`) {
			stats++
		}
	}

	t.Logf("%d/20 STATS replies received, %d malformed frames", stats, corrupt)
	if stats != 20 {
		t.Errorf("expected 20 STATS replies, got %d", stats)
	}
	if corrupt > 0 {
		t.Errorf("%d malformed frames", corrupt)
	}
}

// The pre-handshake health check still works, and still needs no handshake.
func TestSrvStatsBeforeHandshake(t *testing.T) {
	p := dial(t)
	defer p.close()
	p.send(`{"type":"STATS"}`)

	f := p.recv(2 * time.Second)
	if f == "" {
		t.Fatal("no reply to pre-handshake STATS")
	}
	var reply statsReplyPacket
	if err := json.Unmarshal([]byte(f), &reply); err != nil {
		t.Fatalf("reply is not valid JSON: %v", err)
	}
	if reply.Type != PacketStats {
		t.Fatalf("expected a STATS reply, got %q", reply.Type)
	}
	t.Logf("pre-handshake STATS answered: onlineCount=%d uniqueCount=%d", reply.OnlineCount, reply.UniqueCount)
}
