// Copyright 2015 The go-ethereum Authors
// This file is part of The go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with The go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"crypto/ecdsa"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/CortexFoundation/CortexTheseus/common/mclock"
	"github.com/CortexFoundation/CortexTheseus/crypto"
	"github.com/CortexFoundation/CortexTheseus/internal/testlog"
	"github.com/CortexFoundation/CortexTheseus/log"
	"github.com/CortexFoundation/CortexTheseus/p2p/enode"
	"github.com/CortexFoundation/CortexTheseus/p2p/enr"
	"github.com/CortexFoundation/CortexTheseus/p2p/netutil"
)

func TestTable_pingReplace(t *testing.T) {
	run := func(newNodeResponding, lastInBucketResponding bool) {
		name := fmt.Sprintf("newNodeResponding=%t/lastInBucketResponding=%t", newNodeResponding, lastInBucketResponding)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testPingReplace(t, newNodeResponding, lastInBucketResponding)
		})
	}

	run(true, true)
	run(false, true)
	run(true, false)
	run(false, false)
}

func testPingReplace(t *testing.T, newNodeIsResponding, lastInBucketIsResponding bool) {
	simclock := new(mclock.Simulated)
	transport := newPingRecorder()
	tab, db := newTestTable(transport, Config{
		Clock: simclock,
		Log:   testlog.Logger(t, log.LevelTrace),
	})
	defer db.Close()
	defer tab.close()

	<-tab.initDone

	// Fill up the sender's bucket.
	replacementNodeKey, _ := crypto.HexToECDSA("45a915e4d060149eb4365960e6a7a45f334393093061116b197e3240065ff2d8")
	replacementNode := wrapNode(enode.NewV4(&replacementNodeKey.PublicKey, net.IP{127, 0, 0, 1}, 99, 99))
	last := fillBucket(tab, replacementNode.ID())
	tab.mutex.Lock()
	nodeEvents := newNodeEventRecorder(128)
	tab.nodeAddedHook = nodeEvents.nodeAdded
	tab.nodeRemovedHook = nodeEvents.nodeRemoved
	tab.mutex.Unlock()

	// The revalidation process should replace
	// this node in the bucket if it is unresponsive.
	transport.dead[last.ID()] = !lastInBucketIsResponding
	transport.dead[replacementNode.ID()] = !newNodeIsResponding

	// Add replacement node to table.
	tab.addFoundNode(replacementNode)

	t.Log("last:", last.ID())
	t.Log("replacement:", replacementNode.ID())

	// Wait until the last node was pinged.
	waitForRevalidationPing(t, transport, tab, last.ID())

	if !lastInBucketIsResponding {
		if !nodeEvents.waitNodeAbsent(last.ID(), 2*time.Second) {
			t.Error("last node was not removed")
		}
		if !nodeEvents.waitNodePresent(replacementNode.ID(), 2*time.Second) {
			t.Error("replacement node was not added")
		}

		// If a replacement is expected, we also need to wait until the replacement node
		// was pinged and added/removed.
		waitForRevalidationPing(t, transport, tab, replacementNode.ID())
		if !newNodeIsResponding {
			if !nodeEvents.waitNodeAbsent(replacementNode.ID(), 2*time.Second) {
				t.Error("replacement node was not removed")
			}
		}
	}

	// Check bucket content.
	tab.mutex.Lock()
	defer tab.mutex.Unlock()
	wantSize := bucketSize
	if !lastInBucketIsResponding && !newNodeIsResponding {
		wantSize--
	}
	bucket := tab.bucket(replacementNode.ID())
	if l := len(bucket.entries); l != wantSize {
		t.Errorf("wrong bucket size after revalidation: got %d, want %d", l, wantSize)
	}
	if ok := contains(bucket.entries, last.ID()); ok != lastInBucketIsResponding {
		t.Errorf("revalidated node found: %t, want: %t", ok, lastInBucketIsResponding)
	}
	wantNewEntry := newNodeIsResponding && !lastInBucketIsResponding
	if ok := contains(bucket.entries, replacementNode.ID()); ok != wantNewEntry {
		t.Errorf("replacement node found: %t, want: %t", ok, wantNewEntry)
	}
}

// waitForRevalidationPing waits until a PING message is sent to a node with the given id.
func waitForRevalidationPing(t *testing.T, transport *pingRecorder, tab *Table, id enode.ID) *enode.Node {
	t.Helper()

	simclock := tab.cfg.Clock.(*mclock.Simulated)
	maxAttempts := tab.len() * 8
	for i := 0; i < maxAttempts; i++ {
		simclock.Run(tab.cfg.PingInterval)
		p := transport.waitPing(2 * time.Second)
		if p == nil {
			t.Fatal("Table did not send revalidation ping")
		}
		if id == (enode.ID{}) || p.ID() == id {
			return p
		}
	}
	t.Fatalf("Table did not ping node %v (%d attempts)", id, maxAttempts)
	return nil
}

// This checks that the table-wide IP limit is applied correctly.
func TestTable_IPLimit(t *testing.T) {
	transport := newPingRecorder()
	tab, db := newTestTable(transport, Config{})
	defer db.Close()
	defer tab.close()

	for i := 0; i < tableIPLimit+1; i++ {
		n := nodeAtDistance(tab.self().ID(), i, net.IP{172, 0, 1, byte(i)})
		tab.addFoundNode(n)
	}
	if tab.len() > tableIPLimit {
		t.Errorf("too many nodes in table")
	}
	checkIPLimitInvariant(t, tab)
}

// This checks that the per-bucket IP limit is applied correctly.
func TestTable_BucketIPLimit(t *testing.T) {
	transport := newPingRecorder()
	tab, db := newTestTable(transport, Config{})
	defer db.Close()
	defer tab.close()

	d := 3
	for i := 0; i < bucketIPLimit+1; i++ {
		n := nodeAtDistance(tab.self().ID(), d, net.IP{172, 0, 1, byte(i)})
		tab.addFoundNode(n)
	}
	if tab.len() > bucketIPLimit {
		t.Errorf("too many nodes in table")
	}
	checkIPLimitInvariant(t, tab)
}

// checkIPLimitInvariant checks that ip limit sets contain an entry for every
// node in the table and no extra entries.
func checkIPLimitInvariant(t *testing.T, tab *Table) {
	t.Helper()

	tabset := netutil.DistinctNetSet{Subnet: tableSubnet, Limit: tableIPLimit}
	for _, b := range tab.buckets {
		for _, n := range b.entries {
			tabset.Add(n.IP())
		}
	}
	if tabset.String() != tab.ips.String() {
		t.Errorf("table IP set is incorrect:\nhave: %v\nwant: %v", tab.ips, tabset)
	}
}

func TestTable_findnodeByID(t *testing.T) {
	t.Parallel()

	test := func(test *closeTest) bool {
		// for any node table, Target and N
		transport := newPingRecorder()
		tab, db := newTestTable(transport, Config{})
		defer db.Close()
		defer tab.close()
		fillTable(tab, test.All, true)

		// check that closest(Target, N) returns nodes
		result := tab.findnodeByID(test.Target, test.N, false).entries
		if hasDuplicates(result) {
			t.Errorf("result contains duplicates")
			return false
		}
		if !sortedByDistanceTo(test.Target, result) {
			t.Errorf("result is not sorted by distance to target")
			return false
		}

		// check that the number of results is min(N, tablen)
		wantN := test.N
		if tlen := tab.len(); tlen < test.N {
			wantN = tlen
		}
		if len(result) != wantN {
			t.Errorf("wrong number of nodes: got %d, want %d", len(result), wantN)
			return false
		} else if len(result) == 0 {
			return true // no need to check distance
		}

		// check that the result nodes have minimum distance to target.
		for _, b := range tab.buckets {
			for _, n := range b.entries {
				if contains(result, n.ID()) {
					continue // don't run the check below for nodes in result
				}
				farthestResult := result[len(result)-1].ID()
				if enode.DistCmp(test.Target, n.ID(), farthestResult) < 0 {
					t.Errorf("table contains node that is closer to target but it's not in result")
					t.Logf("  Target:          %v", test.Target)
					t.Logf("  Farthest Result: %v", farthestResult)
					t.Logf("  ID:              %v", n.ID())
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(test, quickcfg()); err != nil {
		t.Error(err)
	}
}

type closeTest struct {
	Self   enode.ID
	Target enode.ID
	All    []*node
	N      int
}

func (*closeTest) Generate(rand *rand.Rand, size int) reflect.Value {
	t := &closeTest{
		Self:   gen(enode.ID{}, rand).(enode.ID),
		Target: gen(enode.ID{}, rand).(enode.ID),
		N:      rand.Intn(bucketSize),
	}
	for _, id := range gen([]enode.ID{}, rand).([]enode.ID) {
		r := new(enr.Record)
		r.Set(enr.IP(genIP(rand)))
		n := wrapNode(enode.SignNull(r, id))
		n.livenessChecks = 1
		t.All = append(t.All, n)
	}
	return reflect.ValueOf(t)
}

func TestTable_addVerifiedNode(t *testing.T) {
	tab, db := newTestTable(newPingRecorder(), Config{})
	<-tab.initDone
	defer db.Close()
	defer tab.close()

	// Insert two nodes.
	n1 := nodeAtDistance(tab.self().ID(), 256, net.IP{88, 77, 66, 1})
	n2 := nodeAtDistance(tab.self().ID(), 256, net.IP{88, 77, 66, 2})
	tab.addFoundNode(n1)
	tab.addFoundNode(n2)
	bucket := tab.bucket(n1.ID())

	// Verify bucket content:
	bcontent := []*node{n1, n2}
	if !reflect.DeepEqual(unwrapNodes(bucket.entries), unwrapNodes(bcontent)) {
		t.Fatalf("wrong bucket content: %v", bucket.entries)
	}

	// Add a changed version of n2.
	newrec := n2.Record()
	newrec.Set(enr.IP{99, 99, 99, 99})
	newn2 := wrapNode(enode.SignNull(newrec, n2.ID()))
	tab.addInboundNode(newn2)

	// Check that bucket is updated correctly.
	newBcontent := []*node{n1, newn2}
	if !reflect.DeepEqual(unwrapNodes(bucket.entries), unwrapNodes(newBcontent)) {
		t.Fatalf("wrong bucket content after update: %v", bucket.entries)
	}
	checkIPLimitInvariant(t, tab)
}

func TestTable_addSeenNode(t *testing.T) {
	tab, db := newTestTable(newPingRecorder(), Config{})
	<-tab.initDone
	defer db.Close()
	defer tab.close()

	// Insert two nodes.
	n1 := nodeAtDistance(tab.self().ID(), 256, net.IP{88, 77, 66, 1})
	n2 := nodeAtDistance(tab.self().ID(), 256, net.IP{88, 77, 66, 2})
	tab.addFoundNode(n1)
	tab.addFoundNode(n2)

	// Verify bucket content:
	bcontent := []*node{n1, n2}
	if !reflect.DeepEqual(tab.bucket(n1.ID()).entries, bcontent) {
		t.Fatalf("wrong bucket content: %v", tab.bucket(n1.ID()).entries)
	}

	// Add a changed version of n2.
	newrec := n2.Record()
	newrec.Set(enr.IP{99, 99, 99, 99})
	newn2 := wrapNode(enode.SignNull(newrec, n2.ID()))
	tab.addFoundNode(newn2)

	// Check that bucket content is unchanged.
	if !reflect.DeepEqual(tab.bucket(n1.ID()).entries, bcontent) {
		t.Fatalf("wrong bucket content after update: %v", tab.bucket(n1.ID()).entries)
	}
	checkIPLimitInvariant(t, tab)
}

// This test checks that ENR updates happen during revalidation. If a node in the table
// announces a new sequence number, the new record should be pulled.
func TestTable_revalidateSyncRecord(t *testing.T) {
	transport := newPingRecorder()
	tab, db := newTestTable(transport, Config{
		Clock: new(mclock.Simulated),
		Log:   testlog.Logger(t, log.LevelTrace),
	})
	<-tab.initDone
	defer db.Close()
	defer tab.close()

	// Insert a node.
	var r enr.Record
	r.Set(enr.IP(net.IP{127, 0, 0, 1}))
	id := enode.ID{1}
	n1 := wrapNode(enode.SignNull(&r, id))
	tab.addFoundNode(n1)

	// Update the node record.
	r.Set(enr.WithEntry("foo", "bar"))
	n2 := enode.SignNull(&r, id)
	transport.updateRecord(n2)

	// Wait for revalidation. We wait for the node to be revalidated two times
	// in order to synchronize with the update in the table.
	waitForRevalidationPing(t, transport, tab, n2.ID())
	waitForRevalidationPing(t, transport, tab, n2.ID())

	intable := tab.getNode(id)
	if !reflect.DeepEqual(intable, n2) {
		t.Fatalf("table contains old record with seq %d, want seq %d", intable.Seq(), n2.Seq())
	}
}

func TestNodesPush(t *testing.T) {
	var target enode.ID
	n1 := nodeAtDistance(target, 255, intIP(1))
	n2 := nodeAtDistance(target, 254, intIP(2))
	n3 := nodeAtDistance(target, 253, intIP(3))
	perm := [][]*node{
		{n3, n2, n1},
		{n3, n1, n2},
		{n2, n3, n1},
		{n2, n1, n3},
		{n1, n3, n2},
		{n1, n2, n3},
	}

	// Insert all permutations into lists with size limit 3.
	for _, nodes := range perm {
		list := nodesByDistance{target: target}
		for _, n := range nodes {
			list.push(n, 3)
		}
		if !slicesEqual(list.entries, perm[0], nodeIDEqual) {
			t.Fatal("not equal")
		}
	}

	// Insert all permutations into lists with size limit 2.
	for _, nodes := range perm {
		list := nodesByDistance{target: target}
		for _, n := range nodes {
			list.push(n, 2)
		}
		if !slicesEqual(list.entries, perm[0][:2], nodeIDEqual) {
			t.Fatal("not equal")
		}
	}
}

func nodeIDEqual(n1, n2 *node) bool {
	return n1.ID() == n2.ID()
}

func slicesEqual[T any](s1, s2 []T, check func(e1, e2 T) bool) bool {
	if len(s1) != len(s2) {
		return false
	}
	for i := range s1 {
		if !check(s1[i], s2[i]) {
			return false
		}
	}
	return true
}

// gen wraps quick.Value so it's easier to use.
// it generates a random value of the given value's type.
func gen(typ any, rand *rand.Rand) any {
	v, ok := quick.Value(reflect.TypeOf(typ), rand)
	if !ok {
		panic(fmt.Sprintf("couldn't generate random value of type %T", typ))
	}
	return v.Interface()
}

func genIP(rand *rand.Rand) net.IP {
	ip := make(net.IP, 4)
	rand.Read(ip)
	return ip
}

func quickcfg() *quick.Config {
	return &quick.Config{
		MaxCount: 5000,
		Rand:     rand.New(rand.NewSource(time.Now().Unix())),
	}
}

func newkey() *ecdsa.PrivateKey {
	key, err := crypto.GenerateKey()
	if err != nil {
		panic("couldn't generate key: " + err.Error())
	}
	return key
}
