package bedrock

import (
	"encoding/binary"
	"hash/crc32"
)

// EventStreamContentType is the media type of AWS event streams.
const EventStreamContentType = "application/vnd.amazon.eventstream"

// header is one event-stream message header. Only string values are needed.
type header struct {
	name, value string
}

// encodeEvent frames payload as an AWS event-stream message:
//
//	total length (4) | headers length (4) | prelude CRC (4) | headers | payload | message CRC (4)
//
// Every header is a string (type 7). Both CRCs are CRC-32 (IEEE).
func encodeEvent(headers []header, payload []byte) []byte {
	var hdrs []byte
	for _, h := range headers {
		hdrs = append(hdrs, byte(len(h.name)))
		hdrs = append(hdrs, h.name...)
		hdrs = append(hdrs, 7)
		hdrs = binary.BigEndian.AppendUint16(hdrs, uint16(len(h.value)))
		hdrs = append(hdrs, h.value...)
	}
	total := 12 + len(hdrs) + len(payload) + 4
	msg := make([]byte, 0, total)
	msg = binary.BigEndian.AppendUint32(msg, uint32(total))
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(hdrs)))
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	msg = append(msg, hdrs...)
	msg = append(msg, payload...)
	return binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
}

// eventMessage frames one JSON event of the given type.
func eventMessage(eventType string, payload []byte) []byte {
	return encodeEvent([]header{
		{":event-type", eventType},
		{":content-type", "application/json"},
		{":message-type", "event"},
	}, payload)
}
