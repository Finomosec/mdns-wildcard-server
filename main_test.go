package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
)

func configFrom(t *testing.T, records string) *config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.conf")
	if err := os.WriteFile(path, []byte(records), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path, 120)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func unpackAll(t *testing.T, packets [][]byte) []dns.RR {
	t.Helper()
	var rrs []dns.RR
	for i, p := range packets {
		var m dns.Msg
		if err := m.Unpack(p); err != nil {
			t.Fatalf("packet %d does not unpack: %v", i, err)
		}
		if !m.Response || m.Id != 0 || len(m.Question) != 0 {
			t.Errorf("packet %d is no mDNS announcement: %v", i, m.MsgHdr)
		}
		rrs = append(rrs, m.Answer...)
	}
	return rrs
}

func TestAnnouncementsCoverExactNamesOnly(t *testing.T) {
	c := configFrom(t, `
*.apps.local   192.168.1.20
nas.local      192.168.1.10
nas.local      fd00::10
x.apps.local   192.168.1.30
nas.local      192.168.1.99
`)
	got := map[string]string{}
	for _, rr := range unpackAll(t, announcements(c)) {
		h := rr.Header()
		if h.Class != dns.ClassINET|topBit || h.Ttl != 120 {
			t.Errorf("%s: class %#x ttl %d, want cache-flush IN and ttl 120", h.Name, h.Class, h.Ttl)
		}
		key := h.Name + " " + dns.TypeToString[h.Rrtype]
		switch r := rr.(type) {
		case *dns.A:
			got[key] = r.A.String()
		case *dns.AAAA:
			got[key] = r.AAAA.String()
		}
	}
	want := map[string]string{
		"nas.local. A":    "192.168.1.10",
		"nas.local. AAAA": "fd00::10",
		"x.apps.local. A": "192.168.1.20",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("announced %v, want %v (same answers as a query, no wildcards)", got, want)
	}
}

func TestAnnouncementsAreSplitBelowMaxPacketLen(t *testing.T) {
	records := ""
	for i := 0; i < 200; i++ {
		records += fmt.Sprintf("service-with-a-long-name-%03d.local 192.168.1.%d\n", i, i%250)
		records += fmt.Sprintf("service-with-a-long-name-%03d.local fd00::%x\n", i, i)
	}
	packets := announcements(configFrom(t, records))
	if len(packets) < 2 {
		t.Fatalf("got %d packet(s), want the records split over several", len(packets))
	}
	for i, p := range packets {
		if len(p) > maxPacketLen {
			t.Errorf("packet %d has %d bytes, limit %d", i, len(p), maxPacketLen)
		}
	}
	if n := len(unpackAll(t, packets)); n != 400 {
		t.Errorf("announced %d records over all packets, want 400", n)
	}
}
