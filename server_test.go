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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/sjson"
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
	// Deferred: the room recovers panics, so a closure that panics would
	// otherwise never close this and the test would hang instead of failing
	if !r.post(func() { defer close(done); fn() }) {
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

	c := &Client{id: 1, room: r, conn: srv2, sendCh: make(chan string, 4), queued: &atomic.Int64{}, online: true}
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

// Finding 6: console.go indexed args[1] for disable/message/deleteRoom with no
// bounds check, on a goroutine with no recover. Typing "disable" with no id
// panicked processStdin and took the whole server, and every session, down.
func TestSrvConsoleNeverPanics(t *testing.T) {
	lines := []string{
		// the three that used to panic, and their trailing-space variants
		"disable", "message", "deleteRoom",
		"disable ", "message ", "deleteRoom ",
		// arguments that parse but resolve to nothing
		"disable notanumber", "message 0", "message 999999 hi", "deleteRoom nope",
		// everything else, including empty and unknown input
		"disableAll", "messageAll", "roomCount", "clientCount", "stats", "list",
		"help", "", "   ", "quiet", "quiet",
	}

	var logs strings.Builder
	log.SetOutput(&logs)
	for _, line := range lines {
		testServer.runConsoleCommand(strings.Split(line, " "))
	}
	log.SetOutput(os.Stderr)

	if strings.Contains(logs.String(), "Panic in") {
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "Panic in") {
				t.Error(l)
			}
		}
	}

	// The server must still be serving after all of that.
	p := dial(t)
	defer p.close()
	p.send(`{"type":"STATS"}`)
	if p.recv(2*time.Second) == "" {
		t.Fatal("server stopped answering after console input")
	}
	t.Logf("%d console lines run, server still serving", len(lines))
}

// A well-formed command still does its job.
func TestSrvConsoleDeleteRoomStillWorks(t *testing.T) {
	const id = "console-room"
	p := dial(t)
	defer p.close()
	p.handshake(id, 0)
	p.recv(time.Second)

	r := room(t, id)
	testServer.runConsoleCommand([]string{"deleteRoom", id})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		testServer.mu.Lock()
		_, still := testServer.rooms[id]
		testServer.mu.Unlock()
		if !still {
			t.Log("deleteRoom unregistered the room")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = r
	t.Error("deleteRoom did not remove the room")
}

// oldBroadcastPacket is the previous implementation's output for member i:
// join every state, then rewrite the whole snapshot to mark one entry. Kept
// as the reference the optimised splice must match byte for byte.
func oldBroadcastPacket(states []string, i int) string {
	packet := `{"type":"` + PacketAllClientState + `","state":[` + strings.Join(states, ",") + `]}`
	out, _ := sjson.Set(packet, "state."+strconv.Itoa(i)+".self", true)
	return out
}

// Finding 1: the spliced snapshot must be byte-identical to what the previous
// implementation produced, for every member, including states with commas and
// brackets inside strings and states sjson cannot mark.
func TestSrvBroadcastByteIdenticalToOld(t *testing.T) {
	cases := [][]string{
		{`{"clientId":1}`},
		{`{"clientId":1}`, `{"clientId":2}`},
		{`{"clientId":1,"a":"x"}`, `{"clientId":2}`, `{"clientId":3,"n":{"b":[1,2,3]}}`},
		{`{"clientId":1,"s":"has,comma"}`, `{"clientId":2,"s":"]}"}`},
		{`{"clientId":1,"s":"{\"nested\":\"json\"}"}`, `{"clientId":2}`},
		{`{}`, `{"clientId":2}`, `{}`},
		{`[1,2]`, `{"clientId":2}`}, // sjson refuses arrays; both forms must agree
	}

	for n, states := range cases {
		snapshot := newClientStateSnapshot(states)
		for i := range states {
			got := snapshot.packetFor(i)
			want := oldBroadcastPacket(states, i)
			if got != want {
				t.Errorf("case %d member %d:\n got: %s\nwant: %s", n, i, got, want)
			}
		}
	}
	t.Logf("%d snapshots match the previous implementation byte for byte", len(cases))
}

// Whatever the states, the packet stays parseable and marks exactly one self.
func TestSrvBroadcastPacketShape(t *testing.T) {
	states := []string{
		`{"clientId":1,"scene":3}`,
		`{"clientId":2,"items":{"a":1,"b":[1,2]}}`,
		`{"clientId":3}`,
	}
	snapshot := newClientStateSnapshot(states)

	for i := range states {
		packet := snapshot.packetFor(i)
		var decoded struct {
			Type  string           `json:"type"`
			State []map[string]any `json:"state"`
		}
		if err := json.Unmarshal([]byte(packet), &decoded); err != nil {
			t.Fatalf("member %d: invalid JSON: %v\n%s", i, err, packet)
		}
		if decoded.Type != PacketAllClientState {
			t.Errorf("member %d: type = %q", i, decoded.Type)
		}
		if len(decoded.State) != len(states) {
			t.Fatalf("member %d: %d entries, want %d", i, len(decoded.State), len(states))
		}
		for j, entry := range decoded.State {
			isSelf := entry["self"] == true
			if isSelf != (j == i) {
				t.Errorf("member %d: entry %d self=%v, want %v", i, j, isSelf, j == i)
			}
		}
	}
	t.Log("exactly one self marker, on the right entry, in every packet")
}

// The whole point: allocation should track what is sent, not N times it.
func BenchmarkBroadcastAllClientState(b *testing.B) {
	for _, n := range []int{8, 16, 32} {
		states := make([]string, n)
		for i := range states {
			states[i] = `{"clientId":` + strconv.Itoa(i) + `,"pad":"` + strings.Repeat("x", 32*1024) + `"}`
		}
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				snapshot := newClientStateSnapshot(states)
				for j := range states {
					_ = snapshot.packetFor(j)
				}
			}
		})
		b.Run("N="+strconv.Itoa(n)+"/old", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := range states {
					_ = oldBroadcastPacket(states, j)
				}
			}
		})
	}
}

// fakeConn stands in for a live connection in tests that never write to one.
type fakeConn struct{ net.Conn }

func (fakeConn) Close() error                       { return nil }
func (fakeConn) SetWriteDeadline(t time.Time) error { return nil }

func teamID(s string) *string { return &s }

// A queue-full teardown happens inside the snapshot send, and that teardown
// broadcasts again. Those must not nest once per dropped client.
func TestSrvBroadcastDoesNotNestPerDrop(t *testing.T) {
	for _, n := range []int{8, 16, 32} {
		r := NewRoom(testServer, "nest-"+strconv.Itoa(n), 1, "{}")
		state := `{"clientId":0,"pad":"` + strings.Repeat("x", 4*1024) + `"}`
		for i := 1; i <= n; i++ {
			// unbuffered: every send takes the "queue full" branch
			r.clients[uint64(i)] = &Client{
				id: uint64(i), room: r, state: state,
				sendCh: make(chan string), queued: &atomic.Int64{}, conn: fakeConn{}, online: true,
			}
		}

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		r.broadcastAllClientState()
		runtime.ReadMemStats(&after)
		kb := (after.TotalAlloc - before.TotalAlloc) / 1024

		// At most two passes, each sending n packets of n states. Allow 3x for
		// builder growth and the per-member sjson.Set. The nesting version blew
		// through this by an order of magnitude (~170 MB at n=32).
		budget := uint64(3*2*n*n*len(state)) / 1024
		t.Logf("N=%-3d every queue full -> %5d KB for one broadcast (budget %d KB)", n, kb, budget)
		if kb > budget {
			t.Errorf("N=%d allocated %d KB, expected under %d KB — broadcasts are still nesting", n, kb, budget)
		}
		for _, c := range r.clients {
			if c.online {
				t.Errorf("client %d should have been dropped", c.id)
			}
		}
	}
}

// The replay queue must be bounded in bytes, not just in packet count.
func TestSrvTeamQueueBoundedInBytes(t *testing.T) {
	r := NewRoom(testServer, "bytes-room", 1, "{}")
	team := r.findOrCreateTeam("t")

	packet := `{"type":"J","pad":"` + strings.Repeat("z", 256*1024) + `"}`
	for i := 0; i < MAX_TEAM_QUEUE*2; i++ {
		team.enqueue(packet)
	}

	actual := 0
	for _, q := range team.queue {
		actual += len(q)
	}
	t.Logf("%d packets of %d KB enqueued -> %d retained, %d MB (bound %d MB), %d dropped",
		MAX_TEAM_QUEUE*2, len(packet)/1024, len(team.queue), actual/(1<<20),
		MAX_TEAM_QUEUE_BYTES/(1<<20), team.droppedFromQueue)

	if actual > MAX_TEAM_QUEUE_BYTES {
		t.Errorf("queue holds %d bytes, over the %d byte bound", actual, MAX_TEAM_QUEUE_BYTES)
	}
	if team.queueBytes != actual {
		t.Errorf("queueBytes = %d, actual = %d — accounting drifted", team.queueBytes, actual)
	}
	if len(team.queue) > MAX_TEAM_QUEUE {
		t.Errorf("queue holds %d packets, over the %d packet bound", len(team.queue), MAX_TEAM_QUEUE)
	}
	if team.droppedFromQueue == 0 {
		t.Error("expected oldest packets to be dropped")
	}
}

// A single packet larger than the byte bound is still kept, not dropped into
// an empty queue, and does not break the accounting.
func TestSrvTeamQueueKeepsOversizePacket(t *testing.T) {
	team := &Team{id: "t", state: "{}"}
	big := strings.Repeat("q", MAX_TEAM_QUEUE_BYTES+1024)
	team.enqueue(big)
	if len(team.queue) != 1 || team.queueBytes != len(big) {
		t.Fatalf("oversize packet: queue=%d bytes=%d", len(team.queue), team.queueBytes)
	}
	team.enqueue("small")
	if team.queueBytes != len("small") || len(team.queue) != 1 {
		t.Fatalf("after a small packet: queue=%d bytes=%d, want the oversize one evicted",
			len(team.queue), team.queueBytes)
	}
	t.Log("oversize packet retained, then evicted by the next enqueue")
}

// Building the REQUEST_TEAM_STATE reply must not allocate a giant string only
// to discover it is over the limit.
func TestSrvTeamReplyDoesNotOverAllocate(t *testing.T) {
	r := NewRoom(testServer, "reply-room", 1, "{}")
	team := r.findOrCreateTeam("t")
	packet := `{"type":"J","pad":"` + strings.Repeat("y", 64*1024) + `"}`
	for i := 0; i < MAX_TEAM_QUEUE; i++ {
		team.enqueue(packet)
	}

	c := &Client{id: 1, room: r, state: `{"clientId":1}`, sendCh: make(chan string, 4), queued: &atomic.Int64{}}
	r.clients[1] = c

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r.handleRequestTeamState(c, &Envelope{Type: PacketRequestTeamState, TargetTeamID: teamID("t")})
	runtime.ReadMemStats(&after)

	alloc := after.TotalAlloc - before.TotalAlloc
	sent := <-c.sendCh
	t.Logf("queue %d MB raw -> reply built with %d MB allocated, %d bytes sent",
		team.queueBytes/(1<<20), alloc/(1<<20), len(sent))

	if len(sent) > MAX_PACKET_SIZE {
		t.Errorf("sent %d bytes, over the %d byte limit", len(sent), MAX_PACKET_SIZE)
	}
	// Bounded queue means a bounded reply; allocation should stay within a
	// small multiple of MAX_PACKET_SIZE rather than tracking the raw queue.
	if alloc > 4*MAX_PACKET_SIZE {
		t.Errorf("allocated %d MB building one reply", alloc/(1<<20))
	}
}

// Repeating REQUEST_TEAM_STATE must not grow requestingState without bound.
func TestSrvRequestingStateDeduped(t *testing.T) {
	r := NewRoom(testServer, "req-room", 1, "{}")
	team := r.findOrCreateTeam("t")

	asker := &Client{id: 1, room: r, state: `{"clientId":1}`, sendCh: make(chan string, 8192), queued: &atomic.Int64{}, team: team}
	mate := &Client{id: 2, room: r, state: `{"clientId":2}`, sendCh: make(chan string, 8192), queued: &atomic.Int64{},
		team: team, saveLoaded: true, conn: fakeConn{}}
	r.clients[1], r.clients[2] = asker, mate

	for i := 0; i < 5000; i++ {
		r.handleRequestTeamState(asker, &Envelope{Type: PacketRequestTeamState, TargetTeamID: teamID("t")})
	}

	t.Logf("5000 identical requests -> %d entries in requestingState", len(team.requestingState))
	if len(team.requestingState) != 1 {
		t.Errorf("expected 1 entry, got %d", len(team.requestingState))
	}
}

// #1: shutdown must survive being called before Start publishes the listener,
// and must not be silently swallowed by a recover when it does.
func TestSrvCloseListenerBeforeStart(t *testing.T) {
	s := NewServer() // no Start, so no listener
	s.closeListener()
	s.closeListener() // and again, still no panic
	t.Log("closeListener on a server that never bound is a no-op")
}

// The console path is what actually races startup: processStdin is running
// before Start assigns the listener, and `stop` calls shutdown.
func TestSrvStopBeforeListenerDoesNotPanic(t *testing.T) {
	s := NewServer()

	var logs strings.Builder
	log.SetOutput(&logs)
	// runConsoleCommand's recover would hide a nil deref here, so assert on
	// the log rather than on a panic escaping.
	s.closeListener()
	log.SetOutput(os.Stderr)

	if strings.Contains(logs.String(), "Panic in") {
		t.Errorf("closing an unpublished listener panicked: %s", logs.String())
	}
}

// #2: offline clients are forgotten after CLIENT_RETENTION; present ones stay.
func TestSrvPrunesLongOfflineClients(t *testing.T) {
	r := NewRoom(testServer, "prune-room", 1, "{}")

	// offline and long past retention
	r.clients[1] = &Client{id: 1, room: r, state: `{"clientId":1}`,
		lastActivity: time.Now().Add(-CLIENT_RETENTION - time.Minute)}
	// offline but recent
	r.clients[2] = &Client{id: 2, room: r, state: `{"clientId":2}`,
		lastActivity: time.Now().Add(-time.Minute)}
	// connected, and stale-looking: must never be pruned out from under a
	// live socket, whatever lastActivity says
	r.clients[3] = &Client{id: 3, room: r, state: `{"clientId":3}`, conn: fakeConn{},
		sendCh: make(chan string, 8), queued: &atomic.Int64{}, online: true,
		lastActivity: time.Now().Add(-24 * time.Hour)}

	if !r.pruneStaleClients() {
		t.Fatal("expected the long-offline client to be pruned")
	}
	if _, still := r.clients[1]; still {
		t.Error("client offline past retention was kept")
	}
	if _, ok := r.clients[2]; !ok {
		t.Error("recently offline client was pruned")
	}
	if _, ok := r.clients[3]; !ok {
		t.Error("connected client was pruned")
	}

	if r.pruneStaleClients() {
		t.Error("second pass should find nothing to prune")
	}
	t.Logf("retention %v: 1 forgotten, %d kept", CLIENT_RETENTION, len(r.clients))
}

// A pruned client reconnecting is a fresh member, and the team's saved state
// survives independently of it.
func TestSrvPrunedClientCanRejoin(t *testing.T) {
	const id = "rejoin-room"
	p := dial(t)
	defer p.close()
	p.handshake(id, 960001)
	p.recv(time.Second)

	r := room(t, id)
	onRoom(r, func() {
		r.findOrCreateTeam("1").state = `{"saved":true}`
	})
	p.close()
	time.Sleep(200 * time.Millisecond)

	// Age the disconnected client past retention, then sweep.
	onRoom(r, func() {
		if c := r.clients[960001]; c != nil {
			c.lastActivity = time.Now().Add(-CLIENT_RETENTION - time.Minute)
		}
	})
	onRoom(r, func() { r.pruneStaleClients() })

	var gone bool
	onRoom(r, func() { _, ok := r.clients[960001]; gone = !ok })
	if !gone {
		t.Fatal("client was not pruned")
	}

	q := dial(t)
	defer q.close()
	q.handshake(id, 960001)
	if q.recv(2*time.Second) == "" {
		t.Fatal("pruned client could not rejoin")
	}

	var teamState string
	var members int
	onRoom(r, func() {
		teamState = r.findOrCreateTeam("1").state
		members = len(r.clients)
	})
	if teamState != `{"saved":true}` {
		t.Errorf("team state lost: %q", teamState)
	}
	t.Logf("rejoined as a fresh member (%d in room), team state intact", members)
}

// The per-client send queue must be bounded in bytes, not just in packets:
// sendQueueSize packets of MAX_PACKET_SIZE would be gigabytes per connection.
func TestSrvSendQueueBoundedInBytes(t *testing.T) {
	r := NewRoom(testServer, "sendq-room", 1, "{}")
	slow := &Client{id: 1, room: r, state: `{"clientId":1}`,
		sendCh: make(chan string, sendQueueSize), queued: &atomic.Int64{},
		conn: fakeConn{}, online: true}
	r.clients[1] = slow

	// Hold the channel: disconnect nils the client's reference, so measuring
	// slow.sendCh afterwards would just be len(nil).
	ch := slow.sendCh
	counter := slow.queued

	packet := strings.Repeat("p", 256*1024)
	sends := 0
	for i := 0; i < sendQueueSize && slow.sendCh != nil; i++ {
		slow.send("RELAY", packet, true)
		sends++
	}

	held := 0
	for len(ch) > 0 {
		held += len(<-ch)
	}
	t.Logf("%d sends of %d KB -> %d MB held, counter said %d MB (bound %d MB); online=%v",
		sends, len(packet)/1024, held/(1<<20), counter.Load()/(1<<20),
		MAX_QUEUED_BYTES/(1<<20), slow.online)

	if held > MAX_QUEUED_BYTES {
		t.Errorf("queue held %d bytes, over the %d byte bound", held, MAX_QUEUED_BYTES)
	}
	if held == 0 {
		t.Error("nothing was queued; the test measured the wrong thing")
	}
	if slow.online {
		t.Error("a client that never drains should have been dropped")
	}
	// Without the byte bound this would have been sendQueueSize * 256 KB.
	if sends >= sendQueueSize {
		t.Errorf("filled the whole %d-packet queue; the byte bound never bit", sendQueueSize)
	}
}

// Real-time traffic is small; the byte bound must give it more room than the
// old 256-packet cap, not less, so a brief hiccup doesn't drop live players.
func TestSrvSmallPacketHeadroom(t *testing.T) {
	r := NewRoom(testServer, "headroom-room", 1, "{}")
	c := &Client{id: 1, room: r, state: `{"clientId":1}`,
		sendCh: make(chan string, sendQueueSize), queued: &atomic.Int64{},
		conn: fakeConn{}, online: true}
	r.clients[1] = c

	// A position update is a few hundred bytes.
	update := `{"type":"UPDATE_CLIENT_STATE","quiet":true,"state":{"pos":` + strings.Repeat("0", 200) + `}}`
	accepted := 0
	for i := 0; i < sendQueueSize*2 && c.sendCh != nil; i++ {
		c.send("UPDATE_CLIENT_STATE", update, true)
		if c.sendCh != nil {
			accepted++
		}
	}

	t.Logf("queued %d position updates of %d bytes before the bound bit (old cap was 256 packets)",
		accepted, len(update))
	if accepted <= 256 {
		t.Errorf("only %d small packets buffered, worse than the old 256-packet cap", accepted)
	}
}

// A single oversized packet must never be fatal on an empty queue.
func TestSrvOversizePacketOnEmptyQueue(t *testing.T) {
	r := NewRoom(testServer, "oversize-room", 1, "{}")
	c := &Client{id: 1, room: r, state: `{"clientId":1}`,
		sendCh: make(chan string, sendQueueSize), queued: &atomic.Int64{},
		conn: fakeConn{}, online: true}
	r.clients[1] = c

	c.send("BIG", strings.Repeat("x", MAX_QUEUED_BYTES+1024), false)
	if !c.online || c.sendCh == nil {
		t.Fatal("an oversized packet on an empty queue must not drop the session")
	}
	if len(c.sendCh) != 1 {
		t.Fatalf("expected the packet to be queued, depth=%d", len(c.sendCh))
	}
	t.Log("oversized packet accepted on an empty queue")
}

// Two clients claiming the same clientId in the same room look exactly like one
// client reconnecting, so the second takes the first over. The displaced
// connection's reader is a separate goroutine that lives until its socket
// closes, and it must stop speaking for the client the moment it is displaced —
// otherwise the old session writes its team, position and save flags over the
// new one's, which is what "both players acting strange" looks like in game.
func TestSrvDisplacedConnectionCannotWriteThrough(t *testing.T) {
	const id = "dup-takeover"

	// A never reads, so its writer backs up and its socket stays open long
	// enough for the race to be observable rather than a coin flip
	a := dial(t)
	defer a.close()
	a.handshake(id, 7)

	filler := dial(t)
	defer filler.close()
	filler.handshake(id, 0)
	go filler.drain(30 * time.Second)
	time.Sleep(200 * time.Millisecond)

	pad := strings.Repeat("z", 256*1024)
	for i := 0; i < 40; i++ {
		filler.send(`{"type":"JUNK","pad":%q}`, pad)
	}
	time.Sleep(500 * time.Millisecond)

	r := room(t, id)
	onRoom(r, func() {
		if q := r.clients[7].queued.Load(); q < 1<<20 {
			t.Skipf("only %d KB queued for A; test needs its writer wedged", q/1024)
		}
		// Age A out of ACTIVE_CLIENT_WINDOW so B is treated as a reconnect and
		// takes the session over — an active A would instead be given its own
		// id, which is TestSrvDuplicateIdGetsItsOwnId
		r.clients[7].lastPacket = time.Now().Add(-time.Hour)
	})

	b := dial(t)
	defer b.close()
	b.send(`{"type":"HANDSHAKE","roomId":%q,"clientId":7,"roomState":{},"clientState":{"teamId":"B","isSaveLoaded":true}}`, id)
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 10; i++ {
		a.send(`{"type":"UPDATE_CLIENT_STATE","state":{"teamId":"GHOST","isSaveLoaded":true,"marker":"displaced"}}`)
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	onRoom(r, func() {
		c := r.clients[7]
		t.Logf("client 7 after displaced writes: team=%s state=%s", c.team.id, c.state)
		if strings.Contains(c.state, "displaced") || c.team.id != "B" {
			t.Errorf("displaced connection wrote through into the live session")
		}
	})
}

// A second client configured with the same id as an active player must be
// given its own id rather than fighting for that one. Before this, each side
// kicked the other and reconnected forever, and every round broadcast the whole
// room's membership to everybody in it.
func TestSrvDuplicateIdGetsItsOwnId(t *testing.T) {
	const id = "dup-id"

	a := dial(t)
	defer a.close()
	a.handshake(id, 17)
	go a.drain(20 * time.Second)
	time.Sleep(200 * time.Millisecond)

	b := dial(t)
	defer b.close()
	b.send(`{"type":"HANDSHAKE","roomId":%q,"clientId":17,"roomState":{},"clientState":{"teamId":"B","isSaveLoaded":true}}`, id)
	time.Sleep(300 * time.Millisecond)

	r := room(t, id)
	onRoom(r, func() {
		if len(r.clients) != 2 {
			t.Fatalf("expected both clients to be members, got %d", len(r.clients))
		}
		if a := r.clients[17]; a == nil || a.conn == nil || a.team.id != "1" {
			t.Errorf("the active client lost its session to the duplicate: %+v", a)
		}
		for cid, c := range r.clients {
			if cid == 17 {
				continue
			}
			if c.conn == nil || c.team.id != "B" {
				t.Errorf("duplicate got id %d but no working session: %+v", cid, c)
			}
			t.Logf("duplicate of id 17 was reassigned id %d", cid)
		}
	})
}

// The whole point: two rivals that both reconnect the moment they are kicked —
// which is what the game mod does — must stop trading the id between them.
// Before this they never settled, and every round broadcast the whole room's
// membership to everybody in it.
func TestSrvDuplicateIdDoesNotStartAWar(t *testing.T) {
	const id = "dup-war"

	watcher := dial(t)
	defer watcher.close()
	watcher.handshake(id, 0)
	time.Sleep(200 * time.Millisecond)

	// Two rivals both configured with clientId 7, each redialing whenever its
	// socket dies and sending state updates in between like a live player
	stop := make(chan struct{})
	var reconnects atomic.Int64
	var wg sync.WaitGroup
	for _, team := range []string{"X", "Y"} {
		wg.Add(1)
		go func(team string) {
			defer wg.Done()
			for {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				reconnects.Add(1)
				fmt.Fprintf(c, `{"type":"HANDSHAKE","roomId":%q,"clientId":27,"roomState":{},"clientState":{"teamId":%q,"isSaveLoaded":true}}`+"\x00", id, team)
				for {
					select {
					case <-stop:
						c.Close()
						return
					default:
					}
					if _, err := c.Write([]byte(`{"type":"UPDATE_CLIENT_STATE","state":{"teamId":"` + team + `","isSaveLoaded":true}}` + "\x00")); err != nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				c.Close()
			}
		}(team)
	}

	// Count membership snapshots reaching the uninvolved bystander
	snapshots := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f := watcher.recv(500 * time.Millisecond)
		if f == "" {
			break
		}
		if strings.Contains(f, PacketAllClientState) {
			snapshots++
		}
	}
	close(stop)
	wg.Wait()

	// Three joins is three snapshots. A war reconnects as fast as the network
	// allows and broadcasts every time.
	t.Logf("%d reconnects, %d membership snapshots reached the bystander", reconnects.Load(), snapshots)
	if snapshots > 20 || reconnects.Load() > 6 {
		t.Errorf("%d snapshots from %d reconnects — the rivals are still trading the id",
			snapshots, reconnects.Load())
	}
}

// The takeover exists for reconnects, and giving duplicates their own id must
// not cost a real reconnect its identity — in either of the two shapes it
// arrives in: the socket already closed (process died), or the server not yet
// having noticed (network dropped).
func TestSrvReconnectKeepsItsId(t *testing.T) {
	for i, dropSocket := range []bool{true, false} {
		id := fmt.Sprintf("reconnect-%v", dropSocket)
		// Ids are unique server-wide, so each case needs its own
		cid := uint64(900 + i)

		first := dial(t)
		first.handshake(id, cid)
		time.Sleep(300 * time.Millisecond)
		rm := room(t, id)

		if dropSocket {
			first.close()
			time.Sleep(300 * time.Millisecond)
		} else {
			// Still bound server-side, but silent since it went — which is
			// exactly how a dropped connection looks from here
			defer first.close()
			onRoom(rm, func() { rm.clients[cid].lastPacket = time.Now().Add(-time.Hour) })
		}

		again := dial(t)
		defer again.close()
		again.handshake(id, cid)
		time.Sleep(400 * time.Millisecond)

		onRoom(rm, func() {
			c := rm.clients[cid]
			t.Logf("dropSocket=%v: %d member(s), id %d bound=%v", dropSocket, len(rm.clients), cid, c != nil && c.conn != nil)
			if len(rm.clients) != 1 || c == nil || c.conn == nil {
				t.Errorf("dropSocket=%v: reconnect did not resume id %d", dropSocket, cid)
			}
		})
	}
}
