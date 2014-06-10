package main

import (
	"bytes"
	"encoding/gob"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	incomingQueueSize = 100

	// Gost used a number of fixed-size buffers for incoming messages to limit allocations. This is controlled
	// by udpBufSize and nUDPBufs. Note that gost cannot accept statsd messages larger than udpBufSize.
	// In this case, the total size of buffers for incoming messages is 10e3 * 1000 = 10MB.
	udpBufSize = 10e3
	nUDPBufs   = 1000

	// All TCP connections managed by gost have this keepalive duration applied
	tcpKeepAlivePeriod = 30 * time.Second
)

var (
	configFile = flag.String("conf", "conf.toml", "TOML configuration file")
	conf       *Conf

	bufPool = make(chan []byte, nUDPBufs) // pool of buffers for incoming messagse

	incoming = make(chan *Stat, incomingQueueSize) // incoming stats are passed to the aggregator
	outgoing = make(chan []byte)                   // outgoing Graphite messages

	stats = NewBufferedStats()

	forwardingEnabled  bool                 // Whether configured to forward to another gost
	forwardingStats    = NewBufferedStats() // Counters to be forwarded
	forwardKeyPrefix   = []byte("f|")
	forwardingIncoming chan *Stat          // incoming messages to be forwarded
	forwardingOutgoing = make(chan []byte) // outgoing forwarded messages

	// Whether configured to receive forwarded messages
	forwarderEnabled  bool
	forwarderIncoming = make(chan *BufferedStats, incomingQueueSize) // incoming forwarded messages
	forwardedStats    = NewBufferedStats()

	debugServer = &dServer{}

	// The flushTickers and now are functions that the tests can stub out.
	aggregateFlushTicker           func() <-chan time.Time
	aggregateForwardedFlushTicker  func() <-chan time.Time
	aggregateForwardingFlushTicker func() <-chan time.Time
	now                            func() time.Time = time.Now
)

func init() {
	// Preallocate the UDP buffer pool
	for i := 0; i < nUDPBufs; i++ {
		bufPool <- make([]byte, udpBufSize)
	}
}

type StatType int

const (
	StatCounter StatType = iota
	StatGauge
	StatTimer
	StatSet
)

type Stat struct {
	Type       StatType
	Forward    bool
	Name       string
	Value      float64
	SampleRate float64 // Only for counters
}

// tagToStatType maps a tag (e.g., []byte("c")) to a StatType (e.g., StatCounter).
// NOTE: This used to be a map[string]StatType but was changed for performance reasons.
func tagToStatType(b []byte) (StatType, bool) {
	switch len(b) {
	case 1:
		switch b[0] {
		case 'c':
			return StatCounter, true
		case 'g':
			return StatGauge, true
		case 's':
			return StatSet, true
		}
	case 2:
		if b[0] == 'm' && b[1] == 's' {
			return StatTimer, true
		}
	}
	return 0, false
}

func handleMessages(buf []byte) {
	for _, msg := range bytes.Split(buf, []byte{'\n'}) {
		handleMessage(msg)
	}
	bufPool <- buf[:cap(buf)] // Reset buf's length and return to the pool
}

func handleMessage(msg []byte) {
	if len(msg) == 0 {
		return
	}
	debugServer.Print("[in] ", msg)
	stat, ok := parseStatsdMessage(msg)
	if !ok {
		log.Println("bad message:", string(msg))
		metaInc("errors.bad_message")
		return
	}
	if stat.Forward {
		if stat.Type != StatCounter {
			metaInc("errors.bad_metric_type_for_forwarding")
			return
		}
		forwardingIncoming <- stat
	} else {
		incoming <- stat
	}
}

func clientServer(c *net.UDPConn) error {
	for {
		buf := <-bufPool
		n, _, err := c.ReadFromUDP(buf)
		// TODO: Should we try to recover from such errors?
		if err != nil {
			return err
		}
		metaInc("packets_received")
		if n >= udpBufSize {
			metaInc("errors.udp_message_too_large")
			continue
		}
		go handleMessages(buf[:n])
	}
}

// aggregateForwarded merges forwarded gost messages.
func aggregateForwarded() {
	ticker := aggregateForwardedFlushTicker()
	for {
		select {
		case count := <-forwarderIncoming:
			forwardedStats.Merge(count)
		case <-ticker:
			n, msg := forwardedStats.CreateGraphiteMessage(conf.ForwardedNamespace,
				"distinct_forwarded_metrics_flushed")
			dbg.Printf("Sending %d forwarded stat(s) to graphite.", n)
			outgoing <- msg
			forwardedStats.Clear(!conf.ClearStatsBetweenFlushes)
		}
	}
}

func handleForwarded(c net.Conn) {
	decoder := gob.NewDecoder(c)
	for {
		var counts map[string]float64
		if err := decoder.Decode(&counts); err != nil {
			if err == io.EOF {
				return
			}
			log.Println("Error reading forwarded message:", err)
			metaInc("errors.forwarded_message_read")
			return
		}
		forwarderIncoming <- &BufferedStats{Counts: counts}
	}
}

func forwardServer(listener net.Listener) error {
	for {
		c, err := listener.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Temporary() {
				delay := 10 * time.Millisecond
				log.Printf("Accept error: %v; retrying in %v", e, delay)
				time.Sleep(delay)
				continue
			}
			return err
		}
		go handleForwarded(c)
	}
}

// aggregateForwarding reads incoming forward messages and aggregates them. Every flush interval it forwards
// the collected stats.
func aggregateForwarding() {
	ticker := aggregateForwardingFlushTicker()
	for {
		select {
		case stat := <-forwardingIncoming:
			if stat.Type == StatCounter {
				forwardingStats.AddCount(stat.Name, stat.Value/stat.SampleRate)
			}
		case <-ticker:
			n, msg := forwardingStats.CreateForwardMessage()
			if n > 0 {
				dbg.Printf("Forwarding %d stat(s).", n)
				forwardingOutgoing <- msg
			} else {
				dbg.Println("No stats to forward.")
			}
			// Always delete forwarded stats -- they are cleared/preserved between flushes at the receiving end.
			forwardingStats.Clear(false)
		}
	}
}

// flushForwarding pushes forwarding messages to another gost instance.
func flushForwarding() {
	conn := DialPConn(conf.ForwardingAddr)
	defer conn.Close()
	for msg := range forwardingOutgoing {
		debugMsg := fmt.Sprintf("<binary forwarding message; len = %d bytes>", len(msg))
		debugServer.Print("[forward]", []byte(debugMsg))
		if _, err := conn.Write(msg); err != nil {
			log.Printf("Warning: could not write forwarding message to %s: %s", conf.ForwardingAddr, err)
		}
	}
}

// aggregate reads the incoming messages and aggregates them. It sends them to be flushed every flush
// interval.
func aggregate() {
	ticker := aggregateFlushTicker()
	for {
		select {
		case stat := <-incoming:
			key := stat.Name
			switch stat.Type {
			case StatCounter:
				stats.AddCount(key, stat.Value/stat.SampleRate)
			case StatSet:
				stats.AddSetItem(key, stat.Value)
			case StatGauge:
				stats.SetGauge(key, stat.Value)
			case StatTimer:
				stats.RecordTimer(key, stat.Value)
			}
		case <-ticker:
			n, msg := stats.CreateGraphiteMessage(conf.Namespace, "distinct_metrics_flushed")
			dbg.Printf("Flushing %d stat(s).", n)
			outgoing <- msg
			stats.Clear(!conf.ClearStatsBetweenFlushes)
		}
	}
}

// flush pushes outgoing messages to graphite.
func flush() {
	conn := DialPConn(conf.GraphiteAddr)
	defer conn.Close()
	for msg := range outgoing {
		debugServer.Print("[out] ", msg)
		if _, err := conn.Write(msg); err != nil {
			log.Printf("Warning: could not write message to Graphite at %s: %s", conf.GraphiteAddr, err)
		}
	}
}

// dServer listens on a local tcp port and prints out debugging info to clients that connect.
type dServer struct {
	sync.Mutex
	Clients []net.Conn
}

func (s *dServer) Start(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Println("Listening for debug TCP clients on", addr)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				continue
			}
			s.Lock()
			s.Clients = append(s.Clients, c)
			dbg.Printf("Debug client connected. Currently %d connected client(s).", len(s.Clients))
			s.Unlock()
		}
	}()
	return nil
}

func (s *dServer) closeClient(client net.Conn) {
	for i, c := range s.Clients {
		if c == client {
			s.Clients = append(s.Clients[:i], s.Clients[i+1:]...)
			client.Close()
			dbg.Printf("Debug client disconnected. Currently %d connected client(s).", len(s.Clients))
			return
		}
	}
}

func (s *dServer) Print(tag string, msg []byte) {
	s.Lock()
	defer s.Unlock()
	if len(s.Clients) == 0 {
		return
	}

	closed := []net.Conn{}
	for _, line := range bytes.Split(msg, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(tag), line...)
		msg = append(msg, '\n')
		for _, c := range s.Clients {
			// Set an aggressive write timeout so a slow debug client can't impact performance.
			c.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
			if _, err := c.Write(msg); err != nil {
				closed = append(closed, c)
				continue
			}
		}
		for _, c := range closed {
			s.closeClient(c)
		}
	}
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (l tcpKeepAliveListener) Accept() (net.Conn, error) {
	c, err := l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	if err := c.SetKeepAlive(true); err != nil {
		return nil, err
	}
	if err := c.SetKeepAlivePeriod(tcpKeepAlivePeriod); err != nil {
		return nil, err
	}
	return c, nil
}

func main() {
	flag.Parse()
	parseConf()
	aggregateFlushTicker = func() <-chan time.Time {
		return time.NewTicker(time.Duration(conf.FlushIntervalMS) * time.Millisecond).C
	}
	aggregateForwardedFlushTicker = aggregateFlushTicker
	aggregateForwardingFlushTicker = aggregateFlushTicker

	go flush()
	go aggregate()
	if conf.OSStats != nil {
		go checkOSStats()
	}
	if conf.Scripts != nil {
		go runScripts()
	}

	if forwardingEnabled {
		// Having forwardingIncoming be nil when forwarding is not enabled ensures that gost will crash fast if
		// somehow messages are interpreted as forwarded messages even when forwarding is turned off (which should
		// never happen). Otherwise the behavior would be to fill up the queue and then deadlock.
		forwardingIncoming = make(chan *Stat, incomingQueueSize)
		go flushForwarding()
		go aggregateForwarding()
	}

	if forwarderEnabled {
		log.Println("Listening for forwarded gost messages on", conf.ForwarderListenAddr)
		l, err := net.Listen("tcp", conf.ForwarderListenAddr)
		if err != nil {
			log.Fatal(err)
		}
		listener := tcpKeepAliveListener{l.(*net.TCPListener)}
		go aggregateForwarded()
		go func() { log.Fatal(forwardServer(listener)) }()
	}

	if err := debugServer.Start(conf.DebugPort); err != nil {
		log.Fatal(err)
	}

	udpAddr := fmt.Sprintf("localhost:%d", conf.Port)
	udp, err := net.ResolveUDPAddr("udp", udpAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Listening for UDP client requests on", udp)
	conn, err := net.ListenUDP("udp", udp)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(clientServer(conn))
}
