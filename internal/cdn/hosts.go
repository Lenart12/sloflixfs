package cdn

import (
	"log"
	"net"
	"net/url"
	"sync"
	"time"
)

// hostTTL is how long a host's outcome is remembered.
const hostTTL = time.Hour

var (
	hostMu sync.Mutex
	hosts  = map[string]hostCheck{} // host:port -> last connection outcome (artwork hosts, video servers)
)

type hostCheck struct {
	ok    bool
	at    time.Time
	fails int // consecutive failures
}

// hostStatus returns a host's remembered outcome within hostTTL: a success, or two or more failures in a
// row. A single failure, such as a DNS blip, is retried at once.
func hostStatus(addr string) (ok, known bool) {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hosts[addr]
	return c.ok, (c.ok || c.fails >= 2) && time.Since(c.at) < hostTTL
}

// hostResult records a connection outcome and returns the number of consecutive failures.
func hostResult(addr string, err error) int {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hosts[addr]
	if err != nil {
		c.fails++
	} else {
		c.fails = 0
	}
	c.ok, c.at = err == nil, time.Now()
	hosts[addr] = c
	return c.fails
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Reachable reports whether an artwork URL's host accepts connections. Jellyfin aborts a whole metadata
// refresh when an image download times out, so a dead host must stay out of NFOs.
func Reachable(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Hostname() == "" {
		return false
	}
	addr := hostPort(u)
	if ok, known := hostStatus(addr); known {
		return ok
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err == nil {
		conn.Close()
	}
	if fails := hostResult(addr, err); err != nil {
		log.Printf("cdn: artwork host %s unreachable (%d in a row), left out of NFOs: %v", addr, fails, err)
	}
	return err == nil
}
