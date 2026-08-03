package monitor

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// logStreamDecoder unpacks the Docker Engine's multiplexed log stream framing
// (used by /services/{id}/logs and /containers/{id}/logs):
//
//	[8-byte header][payload]...
//	header: byte0 = stream type (0=stdin, 1=stdout, 2=stderr),
//	        bytes 1-3 unused, bytes 4-7 = uint32 BE payload length.
type logStreamDecoder struct {
	r io.Reader
}

func newLogStreamDecoder(r io.Reader) *logStreamDecoder {
	return &logStreamDecoder{r: r}
}

// next returns the payload of the next frame.
func (d *logStreamDecoder) next() ([]byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[4:8])
	buf := make([]byte, size)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, fmt.Errorf("log frame truncated: %w", err)
	}
	return buf, nil
}

// scanLines reads frames and invokes fn for each complete line (trailing \n
// and \r stripped). Partial lines that end at a frame boundary are carried to
// the next frame, and flushed (without a trailing newline) on EOF.
func (d *logStreamDecoder) scanLines(fn func(line string)) error {
	var lineBuf []byte
	emit := func() {
		fn(strings.TrimRight(string(lineBuf), "\r"))
		lineBuf = lineBuf[:0]
	}
	for {
		payload, err := d.next()
		if err != nil {
			if len(lineBuf) > 0 {
				emit()
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		for _, b := range payload {
			if b == '\n' {
				emit()
			} else {
				lineBuf = append(lineBuf, b)
			}
		}
	}
}
