// statspipe is a metrics pipeline
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	//"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	//"github.com/davecgh/go-spew/spew"
	"github.com/davecheney/profile"
)

//-----------------------------------------------------------------------------

const FlushInterval = time.Duration(10 * time.Second)
const BufSize = 8192

// Metric Types
const Counter = "c"
const Gauge = "g"
const Timer = "ms"

//-----------------------------------------------------------------------------

// Command line flags
var (
	listen   = flag.String("listen", ":8125", "Listener address")
	graphite = flag.String("graphite", "localhost:2003", "Graphite server address")

	// Profiling
	cpuprofile   = flag.Bool("cpuprofile", false, "Enable CPU profiling")
	memprofile   = flag.Bool("memprofile", false, "Enable memory profiling")
	blockprofile = flag.Bool("blockprofile", false, "Enable block profiling")

	debug = flag.Bool("debug", false, "Enable debug mode")
)

//-----------------------------------------------------------------------------
// Data structures

// Metric is a numeric data point
type Metric struct {
	Bucket string
	Value  interface{}
	Type   string
}

// Metrics should be in statsd format. Metric names may not have spaces.
//
//     <metric_name>:<metric_value>|<metric_type>|@<sample_rate>
//
// Note: The sample rate is optional
// var statsPattern = regexp.MustCompile(`[\w\.]+:-?\d+\|(?:c|ms|g)(?:\|\@[\d\.]+)?`)

// In is a channel for processing metrics
var In = make(chan *Metric)

// counters holds all of the counter metrics
var counters = struct {
	sync.RWMutex
	m map[string]int64
}{m: make(map[string]int64)}

// gauges holds all of the gauge metrics
var gauges = struct {
	sync.RWMutex
	m map[string]float64
}{m: make(map[string]float64)}

// Timers is a list of floats
type Timers []float64

// timers holds all of the timer metrics
var timers = struct {
	sync.RWMutex
	m map[string]Timers
}{m: make(map[string]Timers)}

// Internal metrics
type Stats struct {
	RecvMessages uint64

	RecvMetrics    uint64
	SentMetrics    uint64
	InvalidMetrics uint64

	RecvCounters uint64
	SentCounters uint64
	RecvGauges   uint64
	SentGauges   uint64
	RecvTimers   uint64
	SentTimers   uint64
}

var stats = &Stats{}

// TODO: move this to command line option
var Percentiles = []int{5, 95}

//-----------------------------------------------------------------------------

// Implement the sort interface for Timers
func (t Timers) Len() int           { return len(t) }
func (t Timers) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
func (t Timers) Less(i, j int) bool { return t[i] < t[j] }

//-----------------------------------------------------------------------------

// ListenUDP creates a UDP listener
func ListenUDP(addr string) error {
	var buf = make([]byte, 1024)
	ln, err := net.ResolveUDPAddr("udp", addr)

	if err != nil {
		return err
	}

	sock, err := net.ListenUDP("udp", ln)

	if err != nil {
		return err
	}

	log.Printf("Listening on UDP %s\n", ln)

	for {
		n, raddr, err := sock.ReadFromUDP(buf[:])

		if err != nil {
			// TODO: handle error
			continue
		}

		if *debug {
			log.Printf("DEBUG: Received UDP message: bytes=%d client=%s",
				n, raddr)
		}

		go handleUdpMessage(buf)
	}
}

func handleUdpMessage(buf []byte) {
	tokens := bytes.Split(buf, []byte("\n"))

	for _, token := range tokens {
		handleMessage(token)
	}
}

// ListenTCP creates a TCP listener
func ListenTCP(addr string) error {
	l, err := net.Listen("tcp", addr)

	if err != nil {
		return err
	}

	defer l.Close()
	log.Printf("Listening on TCP %s\n", l.Addr())

	for {
		conn, err := l.Accept()

		if err != nil {
			// TODO: handle error
			continue
		}

		go handleConnection(conn)
	}
}

// handleConnection handles a single client connection
func handleConnection(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	// Incoming metrics should be separated by a newline
	for {
		line, err := r.ReadBytes('\n')

		if err != nil {
			if err == io.EOF {
				break
			} else {
				// TODO: handle error
			}
		}

		if *debug {
			log.Printf("DEBUG: Received TCP message: bytes=%d client=%s",
				len(line), conn.RemoteAddr())
		}

		handleMessage(line)
	}
}

// Handle an event message
func handleMessage(buf []byte) {
	atomic.AddUint64(&stats.RecvMessages, 1)

	// According to the statsd protocol, metrics should be separated by a
	// newline. This parser isn't quite as strict since it may be receiving
	// metrics from clients that aren't proper statsd clients (e.g. syslog).
	// In that case, the code tries to remove any client prefix by considering
	// everything after the last space as the list of metrics.

	buf = bytes.TrimSpace(buf)
	i := bytes.LastIndex(buf, []byte(" "))

	if i > -1 {
		buf = buf[i+1 : len(buf)]
	}

	tokens := bytes.Split(buf, []byte("\n"))

	for _, token := range tokens {
		// metrics must have a : and | at a minimum
		if !bytes.Contains(token, []byte(":")) ||
			!bytes.Contains(token, []byte("|")) {
			atomic.AddUint64(&stats.InvalidMetrics, 1)
			continue
		}

		if *debug {
			log.Printf("DEBUG: Parsing metric from token: %q", string(token))
		}

		metric, err := parseMetric(token)

		if err != nil {
			if *debug {
				log.Printf("ERROR: Unable to parse metric %q: %s",
					token, err)
			}

			atomic.AddUint64(&stats.InvalidMetrics, 1)
			continue
		}

		// Send metric off for processing
		In <- metric

		if *debug {
			log.Printf("DEBUG: Queued metric for processing: %+v", metric)
		}
	}
}

// parseMetric parses a raw metric into a Metric struct
func parseMetric(b []byte) (*Metric, error) {
	// Remove any whitespace characters
	b = bytes.TrimSpace(b)

	// Find positions of the various separators
	i := bytes.Index(b, []byte(":"))
	j := bytes.Index(b, []byte("|"))
	k := bytes.Index(b, []byte("@"))
	v := b[i+1 : j]

	// End position of the metric type is the end of the byte slice
	// if no sample rate was sent.
	tEnd := len(b)
	var sampleRate float64 = 1

	// Indicates that a sample rate was sent as part of the metric
	if k > -1 {
		tEnd = k - 1 // Use -1 because of the | before the @
		sr := b[(k + 1):len(b)]
		var err error
		sampleRate, err = strconv.ParseFloat(string(sr), 64)

		if err != nil {
			return nil, err
		}
	}

	m := &Metric{
		Bucket: string(b[0:i]),
		Type:   string(b[j+1 : tEnd]),
	}

	switch m.Type {
	case Counter:
		val, err := strconv.ParseInt(string(v), 10, 64)

		if err != nil {
			return nil, err
		}

		m.Value = int64(float64(val) / sampleRate)

	case Gauge, Timer:
		val, err := strconv.ParseFloat(string(v), 64)

		if err != nil {
			return nil, err
		}

		m.Value = val

	default:
		err := fmt.Errorf("unable to create metric for type %q", m.Type)

		return nil, err
	}

	return m, nil
}

// processMetrics updates new metrics and flushes aggregates to Graphite
func processMetrics() {
	ticker := time.NewTicker(FlushInterval)

	for {
		select {
		case <-ticker.C:
			flushMetrics()
		case m := <-In:
			atomic.AddUint64(&stats.RecvMetrics, 1)

			if *debug {
				log.Printf("DEBUG: Received metric for processing: %+v", m)
			}

			switch m.Type {
			case Counter:
				counters.Lock()
				counters.m[m.Bucket] += m.Value.(int64)
				counters.Unlock()
				atomic.AddUint64(&stats.RecvCounters, 1)

			case Gauge:
				gauges.Lock()
				gauges.m[m.Bucket] = m.Value.(float64)
				gauges.Unlock()
				atomic.AddUint64(&stats.RecvGauges, 1)

			case Timer:
				timers.Lock()
				_, ok := timers.m[m.Bucket]

				if !ok {
					var t Timers
					timers.m[m.Bucket] = t
				}

				timers.m[m.Bucket] = append(timers.m[m.Bucket], m.Value.(float64))
				timers.Unlock()
				atomic.AddUint64(&stats.RecvTimers, 1)

			default:
				if *debug {
					log.Printf("DEBUG: Unable to process unknown metric type %q", m.Type)
				}

			}

			if *debug {
				log.Printf("DEBUG: Finished processing metric: %+v", m)
			}
		}
	}
}

// flushMetrics sends metrics to Graphite
func flushMetrics() {
	var buf bytes.Buffer
	now := time.Now().Unix()

	// Build buffer of stats
	nCounters := flushCounters(&buf, now)
	nGauges := flushGauges(&buf, now)
	nTimers := flushTimers(&buf, now)

	stats.SentMetrics = nCounters + nGauges + nTimers
	stats.SentCounters = nCounters
	stats.SentGauges = nGauges
	stats.SentTimers = nTimers

	log.Printf("STATS: %+v", *stats)

	// Add to internal stats and flush
	fmt.Fprintln(&buf, "statsd.metrics.sent", nCounters+nGauges+nTimers, now)
	fmt.Fprintln(&buf, "statsd.counters.sent", nCounters, now)
	fmt.Fprintln(&buf, "statsd.gauges.sent", nGauges, now)
	fmt.Fprintln(&buf, "statsd.timers.sent", nTimers, now)
	flushInternalStats(&buf, now)

	// Send metrics to Graphite
	sendGraphite(&buf)
}

// flushInternalStats writes the internal stats to the buffer
func flushInternalStats(buf *bytes.Buffer, now int64) {
	//fmt.Fprintf(buf, "statsd.metrics.per_second %d %d\n", v, now)
	fmt.Fprintln(buf, "statsd.metrics.recv",
		atomic.LoadUint64(&stats.RecvMetrics), now)
	fmt.Fprintln(buf, "statsd.counters.recv",
		atomic.LoadUint64(&stats.RecvCounters), now)
	fmt.Fprintln(buf, "statsd.gauges.recv",
		atomic.LoadUint64(&stats.RecvGauges), now)
	fmt.Fprintln(buf, "statsd.timers.recv",
		atomic.LoadUint64(&stats.RecvTimers), now)

	// Clear internal metrics
	atomic.StoreUint64(&stats.RecvMessages, 0)

	atomic.StoreUint64(&stats.RecvMetrics, 0)
	atomic.StoreUint64(&stats.SentMetrics, 0)

	atomic.StoreUint64(&stats.RecvCounters, 0)
	atomic.StoreUint64(&stats.SentCounters, 0)

	atomic.StoreUint64(&stats.RecvGauges, 0)
	atomic.StoreUint64(&stats.SentGauges, 0)

	atomic.StoreUint64(&stats.RecvTimers, 0)
	atomic.StoreUint64(&stats.SentTimers, 0)

}

// flushCounters writes the counters to the buffer
func flushCounters(buf *bytes.Buffer, now int64) uint64 {
	counters.Lock()
	defer counters.Unlock()
	var n uint64

	for k, v := range counters.m {
		fmt.Fprintln(buf, k, v, now)
		delete(counters.m, k)
		n++
	}

	return n
}

// flushGauges writes the gauges to the buffer
func flushGauges(buf *bytes.Buffer, now int64) uint64 {
	gauges.Lock()
	defer gauges.Unlock()
	var n uint64

	for k, v := range gauges.m {
		fmt.Fprintln(buf, k, v, now)
		delete(gauges.m, k)
		n++
	}

	return n
}

// flushTimers writes the timers and aggregate statistics to the buffer
func flushTimers(buf *bytes.Buffer, now int64) uint64 {
	timers.RLock()
	defer timers.RUnlock()
	var n uint64

	for k, t := range timers.m {
		count := len(t)

		// Skip processing if there are no timer values
		if count < 1 {
			break
		}

		var sum float64

		for _, v := range t {
			sum += v
		}

		// Linear average (mean)
		mean := float64(sum) / float64(count)

		// Min and Max
		sort.Sort(t)
		min := t[0]
		max := t[len(t)-1]

		// Write out all derived stats
		fmt.Fprintf(buf, "%s.count %d %d\n", k, count, now)
		fmt.Fprintf(buf, "%s.mean %f %d\n", k, mean, now)
		fmt.Fprintf(buf, "%s.lower %f %d\n", k, min, now)
		fmt.Fprintf(buf, "%s.upper %f %d\n", k, max, now)

		// Calculate and write out percentiles
		for _, pct := range Percentiles {
			p := perc(t, pct)
			fmt.Fprintf(buf, "%s.perc%d %f %d\n", k, pct, p, now)
		}

		delete(timers.m, k)
		n += (4 + uint64(len(Percentiles)))
	}

	return n
}

// percentile calculates Nth percentile of a list of values
func perc(values []float64, pct int) float64 {
	p := float64(pct) / float64(100)
	n := float64(len(values))
	i := math.Ceil(p*n) - 1

	return values[int(i)]
}

// sendGraphite sends metrics to graphite
func sendGraphite(buf *bytes.Buffer) {
	log.Printf("Sending metrics to Graphite: bytes=%d host=%s",
		buf.Len(), *graphite)
	t0 := time.Now()

	conn, err := net.Dial("tcp", *graphite)

	if err != nil {
		log.Printf("ERROR: Unable to connect to graphite: %s", err)
		return
	}

	w := bufio.NewWriter(conn)
	n, err := buf.WriteTo(w)

	if err != nil {
		log.Printf("ERROR: Unable to write to graphite: %s", err)
	}

	w.Flush()
	conn.Close()

	log.Printf("Finished sending metrics to Graphite: bytes=%d host=%s duration=%s",
		n, conn.RemoteAddr(), time.Now().Sub(t0))
}

//-----------------------------------------------------------------------------

func main() {
	flag.Parse()

	// Profiling
	if *cpuprofile || *memprofile || *blockprofile {
		cfg := profile.Config{
			CPUProfile:   *cpuprofile,
			MemProfile:   *memprofile,
			BlockProfile: *blockprofile,
			ProfilePath:  ".",
		}

		p := profile.Start(&cfg)
		defer p.Stop()
	}

	// Process metrics as they arrive
	go processMetrics()

	// Setup listeners
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		log.Fatal(ListenUDP(*listen))
	}()

	go func() {
		defer wg.Done()
		log.Fatal(ListenTCP(*listen))
	}()

	wg.Wait()
}
