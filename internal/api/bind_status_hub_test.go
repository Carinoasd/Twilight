package api

import "testing"

func TestBindHubOnlyNotifiesAndUnsubscribes(t *testing.T) {
	hub := newBindStatusHub()
	updates, unsubscribe := hub.subscribe("ABCDEF")
	hub.notify("abcdef")
	select {
	case <-updates:
	default:
		t.Fatal("missing notification")
	}
	unsubscribe()
	unsubscribe()
	if len(hub.watchers) != 0 {
		t.Fatal("watcher leaked")
	}
}
