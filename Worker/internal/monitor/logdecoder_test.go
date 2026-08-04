package monitor

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// frame builds one Docker log-stream frame: [1 byte type][3 pad][uint32 BE size][payload].
func frame(streamType byte, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = streamType
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

func TestLogStreamDecoderLines(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, []byte("hello world\n")))
	buf.Write(frame(2, []byte("error line\n")))
	buf.Write(frame(1, []byte("multi")))   // partial line
	buf.Write(frame(1, []byte("-part\n"))) // continued
	buf.Write(frame(1, []byte("no trailing newline")))

	dec := newLogStreamDecoder(&buf)
	var got []string
	if err := dec.scanLines(func(line string) { got = append(got, line) }); err != nil {
		t.Fatalf("scanLines: %v", err)
	}
	want := []string{"hello world", "error line", "multi-part", "no trailing newline"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogStreamDecoderCarriageReturn(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, []byte("line1\r\n")))
	dec := newLogStreamDecoder(&buf)
	var got string
	_ = dec.scanLines(func(line string) { got = line })
	if got != "line1" {
		t.Errorf("got %q, want %q", got, "line1")
	}
}

func TestLogStreamDecoderTruncated(t *testing.T) {
	b := frame(1, []byte("short"))
	b = b[:8+3] // cut payload
	dec := newLogStreamDecoder(bytes.NewReader(b))
	err := dec.scanLines(func(string) {})
	if err == nil {
		t.Fatal("expected error for truncated frame")
	}
}

func TestLogStreamDecoderEmpty(t *testing.T) {
	dec := newLogStreamDecoder(bytes.NewReader(nil))
	err := dec.scanLines(func(string) {})
	if err != nil {
		t.Fatalf("empty stream should yield nil error, got %v", err)
	}
}
