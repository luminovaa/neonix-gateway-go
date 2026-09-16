package kiro

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEventStreamDecoderAndParser(t *testing.T) {
	frame := kiroTestFrame("assistantResponseEvent", []byte(`{"content":"hello"}`))
	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	typ, payload, err := decoder.Decode()
	require.NoError(t, err)
	require.Equal(t, "assistantResponseEvent", typ)
	parser := &StreamParser{}
	events, err := parser.Parse(typ, payload)
	require.NoError(t, err)
	require.Equal(t, "hello", events[0].Content)
}

func TestEventStreamDecoderRejectsCorruptCRC(t *testing.T) {
	frame := kiroTestFrame("assistantResponseEvent", []byte(`{}`))
	frame[len(frame)-1] ^= 0xff
	_, _, err := NewEventStreamDecoder(bytes.NewReader(frame)).Decode()
	require.ErrorContains(t, err, "CRC mismatch")
}

func kiroTestFrame(eventType string, payload []byte) []byte {
	name := []byte(":event-type")
	value := []byte(eventType)
	headers := []byte{byte(len(name))}
	headers = append(headers, name...)
	headers = append(headers, 7, byte(len(value)>>8), byte(len(value)))
	headers = append(headers, value...)
	total := 12 + len(headers) + len(payload) + 4
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}
