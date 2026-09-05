package session

import (
	"testing"
)

func TestParseClientsSeparatesReadonlyFromWritable(t *testing.T) {
	out := "/dev/pts/63|1|1788311600\n/dev/pts/64|0|1788311700\n"
	got, err := parseClients(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d clients, want 2", len(got))
	}
	if got[0].ID != "/dev/pts/63" || got[0].Writable {
		t.Errorf("client 0 = %+v, want the read-only /dev/pts/63", got[0])
	}
	if got[1].ID != "/dev/pts/64" || !got[1].Writable {
		t.Errorf("client 1 = %+v, want the writable /dev/pts/64", got[1])
	}
	if got[0].AttachedAt == "" {
		t.Error("AttachedAt must be populated from client_created")
	}
}

func TestParseClientsOnNoClients(t *testing.T) {
	got, err := parseClients("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d clients, want 0", len(got))
	}
}

func TestFirstWritable(t *testing.T) {
	clients := []SessionClient{
		{ID: "a", Writable: false},
		{ID: "b", Writable: true},
	}
	if got := firstWritable(clients); got == nil || got.ID != "b" {
		t.Errorf("firstWritable = %v, want the client b", got)
	}
	if got := firstWritable(clients[:1]); got != nil {
		t.Errorf("firstWritable = %v, want nil when every client is read-only", got)
	}
}
