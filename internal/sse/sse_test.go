package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, in string) []Event {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	var out []Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		out = append(out, ev)
	}
}

func TestReaderBasic(t *testing.T) {
	in := "event: message_start\ndata: {\"a\":1}\n\n: comment\n\ndata: line1\r\ndata: line2\r\n\r\ndata: [DONE]\n\n"
	evs := readAll(t, in)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}
	if evs[0].Name != "message_start" || string(evs[0].Data) != `{"a":1}` {
		t.Errorf("event 0 = %+v", evs[0])
	}
	if string(evs[1].Data) != "line1\nline2" {
		t.Errorf("event 1 data = %q, want multi-line", evs[1].Data)
	}
	if string(evs[2].Data) != "[DONE]" {
		t.Errorf("event 2 data = %q", evs[2].Data)
	}
}

func TestReaderTruncatedLastEvent(t *testing.T) {
	evs := readAll(t, "data: partial")
	if len(evs) != 1 || string(evs[0].Data) != "partial" {
		t.Fatalf("got %+v, want one partial event", evs)
	}
}

func TestReaderNoSpaceAfterColon(t *testing.T) {
	evs := readAll(t, "event:ping\ndata:{}\n\n")
	if len(evs) != 1 || evs[0].Name != "ping" || string(evs[0].Data) != "{}" {
		t.Fatalf("got %+v", evs)
	}
}

func TestWriterRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	in := Event{Name: "content_block_delta", Data: []byte("a\nb")}
	if err := w.WriteEvent(in); err != nil {
		t.Fatal(err)
	}
	want := "event: content_block_delta\ndata: a\ndata: b\n\n"
	if buf.String() != want {
		t.Fatalf("written = %q, want %q", buf.String(), want)
	}
	evs := readAll(t, buf.String())
	if len(evs) != 1 || evs[0].Name != in.Name || string(evs[0].Data) != "a\nb" {
		t.Fatalf("round trip = %+v", evs)
	}
}
