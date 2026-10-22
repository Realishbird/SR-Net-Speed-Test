package api

import (
	"reflect"
	"testing"
)

func TestRoutableAddrs(t *testing.T) {
	got := RoutableAddrs([]string{"169.254.42.134/16", "172.27.101.20/24", "fe80::1/64", "bad"})
	if want := []string{"172.27.101.20/24", "bad"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := RoutableAddrs([]string{"169.254.10.55/16"}); len(got) != 0 {
		t.Fatalf("a port with only a link-local address should have none, got %v", got)
	}
	if RoutableAddrs(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
