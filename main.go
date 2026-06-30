// mdns-wildcard-server — a tiny mDNS server (proxy responder).
//
// Answers mDNS queries (.local, multicast 224.0.0.251:5353 and ff02::fb:5353) for
// configured names/wildcards from a static config (NO upstream lookup). The config
// is hot-reloaded automatically via fsnotify.
//
// Purpose: make a namespace such as *.apps.local resolvable on the LAN even though
// the target host doesn't speak mDNS itself or lives in a different subnet
// (a classic proxy responder, in the spirit of a Bonjour proxy).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

const (
	mdnsGroup4 = "224.0.0.251"
	mdnsGroup6 = "ff02::fb"
	mdnsPort   = 5353
	// Top bit: in the qclass it is QU (unicast response requested); in the rrclass
	// of an answer it is the cache-flush bit.
	topBit = 0x8000
	// Legacy unicast answers should use a short TTL (RFC 6762 §6.7).
	legacyMaxTTL = 10
)

type rule struct {
	wildcard bool   // true => suffix match (*.suffix), false => exact name
	name     string // lowercase, no trailing dot; for a wildcard the suffix domain
	ip       net.IP
	v4       bool
}

type config struct {
	rules []rule
	ttl   uint32
}

var (
	cur     atomic.Pointer[config]
	verbose bool
)

func main() {
	cfgPath := flag.String("config", "records.conf", "path to the records file (hot-reloaded)")
	ifName := flag.String("iface", "", "network interface to use (e.g. eth0); empty = OS default")
	ttl := flag.Uint("ttl", 120, "answer TTL in seconds")
	flag.BoolVar(&verbose, "v", false, "verbose logging (log every answered query)")
	flag.Parse()

	var ifi *net.Interface
	if *ifName != "" {
		var err error
		if ifi, err = net.InterfaceByName(*ifName); err != nil {
			log.Fatalf("interface %q not found: %v", *ifName, err)
		}
	}

	c, err := loadConfig(*cfgPath, uint32(*ttl))
	if err != nil {
		log.Fatalf("loading config %q failed: %v", *cfgPath, err)
	}
	cur.Store(c)
	log.Printf("start: %d rule(s) from %q, TTL %ds, iface=%s", len(c.rules), *cfgPath, *ttl, ifaceName(ifi))

	go watchConfig(*cfgPath, uint32(*ttl))

	// IPv4 is required; IPv6 is best-effort (some hosts have no usable v6).
	conn4, group4, err := openMulticast4(ifi)
	if err != nil {
		log.Fatalf("opening IPv4 mDNS socket failed: %v", err)
	}
	defer conn4.Close()
	go serve(conn4, group4)

	if conn6, group6, err := openMulticast6(ifi); err != nil {
		log.Printf("IPv6 mDNS disabled: %v", err)
	} else {
		defer conn6.Close()
		go serve(conn6, group6)
		log.Printf("IPv6 mDNS enabled (%s)", mdnsGroup6)
	}

	select {} // block forever; serve loops run in goroutines
}

// listenReuse binds a UDP socket with SO_REUSEADDR/REUSEPORT so we can coexist with
// avahi & co. on port 5353.
func listenReuse(network, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); e != nil {
				serr = e
				return
			}
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	_ = conn.SetReadBuffer(1 << 20)
	return conn, nil
}

// openMulticast4 binds 0.0.0.0:5353 and joins the IPv4 mDNS group.
//
// Important: do NOT bind to the group address (as net.ListenMulticastUDP does) —
// otherwise outgoing answers would carry an invalid multicast source IP and get
// dropped by other responders (avahi). Binding to 0.0.0.0 + SetMulticastInterface
// lets the kernel pick the correct unicast source IP of the sending interface.
func openMulticast4(ifi *net.Interface) (*net.UDPConn, *net.UDPAddr, error) {
	conn, err := listenReuse("udp4", fmt.Sprintf("0.0.0.0:%d", mdnsPort))
	if err != nil {
		return nil, nil, err
	}
	p := ipv4.NewPacketConn(conn)
	if err := p.JoinGroup(ifi, &net.UDPAddr{IP: net.ParseIP(mdnsGroup4)}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("JoinGroup: %w", err)
	}
	if ifi != nil {
		if err := p.SetMulticastInterface(ifi); err != nil {
			log.Printf("warning: SetMulticastInterface(%s) v4: %v", ifi.Name, err)
		}
	}
	_ = p.SetMulticastLoopback(true)
	return conn, &net.UDPAddr{IP: net.ParseIP(mdnsGroup4), Port: mdnsPort}, nil
}

// openMulticast6 binds [::]:5353 and joins the IPv6 mDNS group (link-local).
func openMulticast6(ifi *net.Interface) (*net.UDPConn, *net.UDPAddr, error) {
	conn, err := listenReuse("udp6", fmt.Sprintf("[::]:%d", mdnsPort))
	if err != nil {
		return nil, nil, err
	}
	p := ipv6.NewPacketConn(conn)
	if err := p.JoinGroup(ifi, &net.UDPAddr{IP: net.ParseIP(mdnsGroup6)}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("JoinGroup: %w", err)
	}
	zone := ""
	if ifi != nil {
		zone = ifi.Name
		if err := p.SetMulticastInterface(ifi); err != nil {
			log.Printf("warning: SetMulticastInterface(%s) v6: %v", ifi.Name, err)
		}
	}
	_ = p.SetMulticastLoopback(true)
	// Link-local group needs the interface zone for sending.
	return conn, &net.UDPAddr{IP: net.ParseIP(mdnsGroup6), Port: mdnsPort, Zone: zone}, nil
}

func serve(conn *net.UDPConn, group *net.UDPAddr) {
	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		var q dns.Msg
		if err := q.Unpack(buf[:n]); err != nil {
			continue // not a valid DNS packet
		}
		if q.Response || len(q.Question) == 0 {
			continue // we only care about queries
		}
		handleQuery(conn, group, &q, src)
	}
}

func handleQuery(conn *net.UDPConn, group *net.UDPAddr, q *dns.Msg, src *net.UDPAddr) {
	c := cur.Load()

	// Legacy unicast: query did NOT come from an mDNS port (5353) -> a classic
	// unicast resolver. Answer unicast, mirror the question, no cache-flush bit.
	legacy := src.Port != mdnsPort
	unicast := legacy

	ttl := c.ttl
	class := uint16(dns.ClassINET)
	if !legacy {
		class |= topBit // cache-flush for unique records
	} else if ttl > legacyMaxTTL {
		ttl = legacyMaxTTL
	}

	var answers []dns.RR
	for _, qd := range q.Question {
		switch qd.Qtype {
		case dns.TypeA, dns.TypeAAAA, dns.TypeANY:
		default:
			continue
		}
		if qd.Qclass&topBit != 0 {
			unicast = true // QU bit set
		}
		name := strings.ToLower(strings.TrimSuffix(qd.Name, "."))
		for _, r := range c.rules {
			if !match(r, name) {
				continue
			}
			if r.v4 && (qd.Qtype == dns.TypeA || qd.Qtype == dns.TypeANY) {
				answers = append(answers, &dns.A{
					Hdr: dns.RR_Header{Name: qd.Name, Rrtype: dns.TypeA, Class: class, Ttl: ttl},
					A:   r.ip,
				})
			}
			if !r.v4 && (qd.Qtype == dns.TypeAAAA || qd.Qtype == dns.TypeANY) {
				answers = append(answers, &dns.AAAA{
					Hdr:  dns.RR_Header{Name: qd.Name, Rrtype: dns.TypeAAAA, Class: class, Ttl: ttl},
					AAAA: r.ip,
				})
			}
			break // first matching rule wins
		}
	}
	if len(answers) == 0 {
		return
	}

	resp := new(dns.Msg)
	resp.Response = true
	resp.Authoritative = true
	resp.Answer = answers
	if legacy {
		// Legacy unicast: mirror ID + question.
		resp.Id = q.Id
		resp.Question = q.Question
	}
	// Genuine mDNS answer: ID 0, no question section (defaults of dns.Msg).

	out, err := resp.Pack()
	if err != nil {
		log.Printf("pack error: %v", err)
		return
	}

	dst := group
	if unicast {
		dst = src
	}
	if _, err := conn.WriteToUDP(out, dst); err != nil {
		log.Printf("write error to %s: %v", dst, err)
		return
	}
	if verbose {
		mode := "multicast"
		if unicast {
			mode = "unicast"
		}
		names := make([]string, 0, len(answers))
		for _, a := range answers {
			names = append(names, a.Header().Name)
		}
		log.Printf("answer (%s) to %s: %s", mode, dst, strings.Join(names, ", "))
	}
}

func match(r rule, name string) bool {
	if r.wildcard {
		// *.suffix matches the suffix itself AND any sub-labels before it.
		return name == r.name || strings.HasSuffix(name, "."+r.name)
	}
	return name == r.name
}

func loadConfig(path string, ttl uint32) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &config{ttl: ttl}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: expected '<pattern> <ip>', got %q", i+1, line)
		}
		pat := strings.ToLower(strings.TrimSuffix(f[0], "."))
		ip := net.ParseIP(f[1])
		if ip == nil {
			return nil, fmt.Errorf("line %d: invalid IP %q", i+1, f[1])
		}
		r := rule{}
		if v4 := ip.To4(); v4 != nil {
			r.ip, r.v4 = v4, true
		} else {
			r.ip = ip
		}
		if strings.HasPrefix(pat, "*.") {
			r.wildcard, r.name = true, strings.TrimPrefix(pat, "*.")
		} else {
			r.name = pat
		}
		c.rules = append(c.rules, r)
	}
	return c, nil
}

// watchConfig reloads the config on file change (fsnotify) or SIGHUP. The parent
// directory is watched so that atomic editor saves (temp file + rename) are caught.
func watchConfig(path string, ttl uint32) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher init failed, hot-reload disabled: %v", err)
		return
	}
	defer w.Close()

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	base := filepath.Base(path)
	if err := w.Add(dir); err != nil {
		log.Printf("watching %q failed, hot-reload disabled: %v", dir, err)
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)

	reload := func(reason string) {
		c, err := loadConfig(path, ttl)
		if err != nil {
			log.Printf("reload (%s) failed — keeping previous config: %v", reason, err)
			return
		}
		cur.Store(c)
		log.Printf("config reloaded (%s): %d rule(s)", reason, len(c.rules))
	}

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) == base && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				reload("file change")
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		case <-sig:
			reload("SIGHUP")
		}
	}
}

func ifaceName(ifi *net.Interface) string {
	if ifi == nil {
		return "(auto)"
	}
	return ifi.Name
}
