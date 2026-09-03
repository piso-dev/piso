package proxy

import (
	"bufio"
	"io"
	"net"
	"testing"
)

func TestHijackedConnReadsBufferedPrefix(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	done := make(chan struct{})
	go func() {
		_, _ = c2.Write([]byte("HELLO-WORLD"))
		_ = c2.Close()
		close(done)
	}()

	br := bufio.NewReader(c1)
	first := make([]byte, 1)
	if _, err := br.Read(first); err != nil {
		t.Fatal(err)
	}
	if first[0] != 'H' {
		t.Fatalf("buffered prefix: got %q", first)
	}

	rest, err := io.ReadAll(&hijackedConn{Conn: c1, br: br})
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "ELLO-WORLD" {
		t.Fatalf("hijackedConn lost buffered bytes: got %q", rest)
	}
	<-done
}
