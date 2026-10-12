package cdn

import (
	"net"
	"testing"
	"time"
)

func TestReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !Reachable("http://" + l.Addr().String() + "/a.jpg") {
		t.Fatal("listening host reported unreachable")
	}
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dead.Close()
	if Reachable("http://" + dead.Addr().String() + "/a.jpg") {
		t.Fatal("closed port reported reachable")
	}
	// One failure is rechecked right away; two in a row hold for hostTTL.
	up := l.Addr().String()
	hosts[up] = hostCheck{at: time.Now(), fails: 1}
	if !Reachable("http://" + up + "/a.jpg") {
		t.Fatal("single failure not rechecked")
	}
	hosts[up] = hostCheck{at: time.Now().Add(-2 * time.Minute), fails: 2}
	if Reachable("http://" + up + "/a.jpg") {
		t.Fatal("two failures in a row should hold for hostTTL")
	}
	if Reachable("data:image/jpeg;base64,AA==") {
		t.Fatal("URL without host reported reachable")
	}
}
