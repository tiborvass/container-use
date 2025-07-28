package proxy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var (
	headerID    = []byte("id:")
	headerData  = []byte("data:")
	headerEvent = []byte("event:")
	headerRetry = []byte("retry:")
)

func trimHeader(size int, data []byte) []byte {
	if data == nil || len(data) < size {
		return data
	}

	data = data[size:]
	// Remove optional leading whitespace
	if len(data) > 0 && data[0] == 32 {
		data = data[1:]
	}
	// Remove trailing new line
	if len(data) > 0 && data[len(data)-1] == 10 {
		data = data[:len(data)-1]
	}
	return data
}

// Event holds all of the event source fields
type Event struct {
	ID        string
	Data      []byte
	Event     string
	Retry     string
	timestamp time.Time
}

// processSSEEvent processes raw SSE data into an Event
// Copied from github.com/r3labs/sse.Client.processEvent
func processSSEEvent(msg []byte, encodingBase64 bool) (*Event, error) {
	var e Event

	if len(msg) < 1 {
		return nil, errors.New("event message was empty")
	}

	// Normalize the crlf to lf to make it easier to split the lines.
	// Split the line by "\n" or "\r", per the spec.
	for _, line := range bytes.FieldsFunc(msg, func(r rune) bool { return r == '\n' || r == '\r' }) {
		switch {
		case bytes.HasPrefix(line, headerID):
			e.ID = string(append([]byte(nil), trimHeader(len(headerID), line)...))
		case bytes.HasPrefix(line, headerData):
			// The spec allows for multiple data fields per event, concatenated them with "\n".
			e.Data = append(e.Data[:], append(trimHeader(len(headerData), line), byte('\n'))...)
		// The spec says that a line that simply contains the string "data" should be treated as a data field with an empty body.
		case bytes.Equal(line, bytes.TrimSuffix(headerData, []byte(":"))):
			e.Data = append(e.Data, byte('\n'))
		case bytes.HasPrefix(line, headerEvent):
			e.Event = string(append([]byte(nil), trimHeader(len(headerEvent), line)...))
		case bytes.HasPrefix(line, headerRetry):
			e.Retry = string(append([]byte(nil), trimHeader(len(headerRetry), line)...))
		default:
			// Ignore any garbage that doesn't match what we're looking for.
		}
	}

	// Trim the last "\n" per the spec.
	e.Data = bytes.TrimSuffix(e.Data, []byte("\n"))

	if encodingBase64 {
		buf := make([]byte, base64.StdEncoding.DecodedLen(len(e.Data)))

		n, err := base64.StdEncoding.Decode(buf, e.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode event message: %s", err)
		}
		e.Data = buf[:n]
	}
	return &e, nil
}
