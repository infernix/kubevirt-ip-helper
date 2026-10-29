package dhcp

// F07 regression tests at the wire level: replacing a pool's options must
// keep the network continuously served. the replacement resolves its ntp
// hostnames before it takes the allocator lock and swaps the pool entry in
// one atomic step, so a valid request which arrives while the resolution
// is still running must be acked with the old options, a replacement which
// AddPool rejects must leave the old pool answering, and only a published
// replacement changes the served options.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// requestForLease issues a valid selecting DHCPREQUEST for the fixture
// lease through the packet handler and returns the parsed reply.
func requestForLease(t *testing.T, a *DHCPAllocator) *dhcpv4.DHCPv4 {
	t.Helper()

	conn := &recordingPacketConn{}
	req := newBootRequest(t, mustHWAddr(t, "aa:bb:cc:dd:ee:01"), dhcpv4.MessageTypeRequest)
	req.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("192.168.0.1")))
	req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("192.168.0.50")))
	a.dhcpHandler("pool1", conn, testPeer(), req)

	if conn.len() != 1 {
		t.Fatalf("expected 1 reply, got %d", conn.len())
	}
	resp, err := dhcpv4.FromBytes(conn.payloads[0])
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}

	return resp
}

// TestReplacementIsAtomicForConcurrentRequests pins the preserved F07
// property at the packet level: a replacement swaps the pool entry in one
// step under the allocator lock, so a request which races the replacement
// is always answered from a complete configuration - the old options or
// the new ones, never a pool-absent nack and never a mix of both. the
// pre-fix code could park a replacement inside its ntp resolution; that
// window is gone with the resolution, and this test keeps the remaining
// lock property honest.
func TestReplacementIsAtomicForConcurrentRequests(t *testing.T) {
	a := newTestPooledAllocator(t)

	oldNTP := net.ParseIP("10.0.0.53")
	newNTP := net.ParseIP("10.0.0.54")

	done := make(chan error, 1)
	go func() {
		defer close(done)
		for range 200 {
			if err := a.AddPool(
				context.Background(),
				"pool1",
				"192.168.0.1",
				"255.255.255.0",
				"192.168.0.254",
				[]string{"1.1.1.1", "8.8.8.8"},
				"example.com",
				[]string{"example.com"},
				[]net.IP{newNTP},
				120,
				"eth0",
			); err != nil {
				done <- err

				return
			}
		}
	}()

	for range 200 {
		resp := requestForLease(t, a)
		if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
			t.Fatalf("message type = %v while the replacement runs, want Ack", mt)
		}

		switch got := resp.IPAddressLeaseTime(0); got {
		case 3600 * time.Second:
			if ntp := resp.NTPServers(); len(ntp) != 1 || !ntp[0].Equal(oldNTP) {
				t.Fatalf("ntp = %v with the old lease time, want [%v]", ntp, oldNTP)
			}
		case 120 * time.Second:
			if ntp := resp.NTPServers(); len(ntp) != 1 || !ntp[0].Equal(newNTP) {
				t.Fatalf("ntp = %v with the new lease time, want [%v]", ntp, newNTP)
			}
		default:
			t.Fatalf("lease time = %v during the replacement, want the old 3600s or the new 120s", got)
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("the replacement did not converge: %v", err)
	}

	// the last replacement is published: the same request is acked with
	// the new options
	resp := requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v after the replacement was published, want Ack", mt)
	}
	if got := resp.IPAddressLeaseTime(0); got != 120*time.Second {
		t.Errorf("lease time = %v after the replacement was published, want the new 120s", got)
	}
}

// TestSetPoolNTPUpdatesOption42 pins the ntp refresh seam of the allocator:
// replacing the addresses of a live pool is visible in the very next reply,
// and the call is a no-op for an absent pool. the served state is copied
// both ways, so neither the caller nor a reader can reach into it.
func TestSetPoolNTPUpdatesOption42(t *testing.T) {
	a := newTestPooledAllocator(t)

	updated := []net.IP{net.ParseIP("10.0.0.54"), net.ParseIP("10.0.0.55")}
	if !a.SetPoolNTP("pool1", updated) {
		t.Fatal("SetPoolNTP reported the registered pool absent")
	}

	resp := requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v, want Ack", mt)
	}
	if got := resp.NTPServers(); len(got) != 2 || !got[0].Equal(updated[0]) || !got[1].Equal(updated[1]) {
		t.Fatalf("ntp = %v after SetPoolNTP, want %v", got, updated)
	}

	// the served addresses are a copy: mutating the caller's buffer must
	// not rewrite option 42
	updated[0] = net.ParseIP("10.9.9.9")
	if got, _ := a.PoolNTP("pool1"); !got[0].Equal(net.ParseIP("10.0.0.54")) {
		t.Errorf("ntp = %v after the caller mutated its buffer, want the served 10.0.0.54", got)
	}

	// the reader gets a copy as well
	read, exists := a.PoolNTP("pool1")
	if !exists {
		t.Fatal("PoolNTP reported the registered pool absent")
	}
	read[0] = net.ParseIP("10.9.9.9")
	if got, _ := a.PoolNTP("pool1"); !got[0].Equal(net.ParseIP("10.0.0.54")) {
		t.Errorf("ntp = %v after the reader mutated its copy, want the served 10.0.0.54", got)
	}

	// an absent pool is a no-op: no entry may be created by a late refresh
	if a.SetPoolNTP("pool-gone", []net.IP{net.ParseIP("10.0.0.56")}) {
		t.Error("SetPoolNTP reported an absent pool as present")
	}
	if a.CheckPool("pool-gone") {
		t.Error("SetPoolNTP registered a pool which does not exist")
	}
	if got, exists := a.PoolNTP("pool-gone"); exists || len(got) != 0 {
		t.Errorf("PoolNTP of an absent pool = %v (exists=%v), want nothing", got, exists)
	}
}

// TestPoolNTPCopiesAddressBytes pins the ownership of the served option 42
// bytes: net.IP is a byte slice, so an outer-slice copy alone would still
// share every address with the caller. mutating an address which was handed
// to AddPool or SetPoolNTP, or an address read back from PoolNTP, must never
// change the bytes of the served packet state.
func TestPoolNTPCopiesAddressBytes(t *testing.T) {
	a := newTestPooledAllocator(t)

	served := net.ParseIP("10.0.0.53").To4()
	read, exists := a.PoolNTP("pool1")
	if !exists {
		t.Fatal("PoolNTP reported the registered pool absent")
	}
	if len(read) != 1 || !read[0].Equal(served) {
		t.Fatalf("served ntp = %v, want [%v]", read, served)
	}

	// the reader's bytes are its own
	read[0][3] = 99
	if got, _ := a.PoolNTP("pool1"); !got[0].Equal(served) {
		t.Errorf("ntp = %v after the reader mutated its copy's bytes, want the served %v", got, served)
	}

	// the replacement's bytes are copied as well
	replacement := net.ParseIP("10.0.0.60").To4()
	if !a.SetPoolNTP("pool1", []net.IP{replacement}) {
		t.Fatal("SetPoolNTP reported the registered pool absent")
	}
	replacement[3] = 99
	if got, _ := a.PoolNTP("pool1"); !got[0].Equal(net.ParseIP("10.0.0.60")) {
		t.Errorf("ntp = %v after the caller mutated the replacement's bytes, want 10.0.0.60", got)
	}

	// the registration's bytes are copied as well
	registered := net.ParseIP("10.0.0.61").To4()
	if err := a.AddPool(
		context.Background(),
		"pool1",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		[]string{"1.1.1.1", "8.8.8.8"},
		"example.com",
		[]string{"example.com"},
		[]net.IP{registered},
		3600,
		"eth0",
	); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	registered[3] = 99

	// the wire observes the served bytes, not the caller's buffer
	resp := requestForLease(t, a)
	if got := resp.NTPServers(); len(got) != 1 || !got[0].Equal(net.ParseIP("10.0.0.61")) {
		t.Errorf("option 42 = %v after the caller mutated the registered address, want [10.0.0.61]", got)
	}
}

// TestRejectedReplacementKeepsTheOldPoolServing pins the F07 rejection
// continuity at the packet level: a replacement which fails AddPool's own
// validation takes no state, so the old pool must still answer requests
// with its options.
func TestRejectedReplacementKeepsTheOldPoolServing(t *testing.T) {
	a := newTestPooledAllocator(t)

	// the rejected replacement: a non-literal dns entry fails AddPool's
	// own validation before any state is taken
	if err := a.AddPool(
		context.Background(),
		"pool1",
		"192.168.0.1",
		"255.255.255.0",
		"192.168.0.254",
		[]string{"dns.example.invalid"},
		"example.com",
		[]string{"example.com"},
		[]net.IP{net.ParseIP("10.0.0.53")},
		120,
		"eth0",
	); err == nil {
		t.Fatal("AddPool accepted a non-literal dns entry, want the rejection")
	}

	// the old pool must still answer with its options
	if !a.CheckPool("pool1") {
		t.Fatal("the rejected replacement unregistered the serving pool")
	}
	resp := requestForLease(t, a)
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeAck {
		t.Fatalf("message type = %v after the rejected replacement, want Ack", mt)
	}
	if got := resp.IPAddressLeaseTime(0); got != 3600*time.Second {
		t.Errorf("lease time = %v after the rejected replacement, want the old 3600s", got)
	}
	if got := resp.DNS(); len(got) != 2 || !got[0].Equal(net.ParseIP("1.1.1.1")) || !got[1].Equal(net.ParseIP("8.8.8.8")) {
		t.Errorf("dns = %v after the rejected replacement, want [1.1.1.1 8.8.8.8]", got)
	}
}
