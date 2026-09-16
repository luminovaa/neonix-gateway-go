package kiro

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

const maxEventStreamFrameBytes = 16 << 20

type EventStreamDecoder struct {
	r *bufio.Reader
}

func NewEventStreamDecoder(r io.Reader) *EventStreamDecoder {
	return &EventStreamDecoder{r: bufio.NewReaderSize(r, 64<<10)}
}

func (d *EventStreamDecoder) Decode() (string, []byte, error) {
	prelude := make([]byte, 12)
	if _, err := io.ReadFull(d.r, prelude); err != nil {
		return "", nil, err
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:]) {
		return "", nil, errors.New("kiro eventstream prelude CRC mismatch")
	}
	total := int(binary.BigEndian.Uint32(prelude[:4]))
	headersLen := int(binary.BigEndian.Uint32(prelude[4:8]))
	if total < 16 || total > maxEventStreamFrameBytes || headersLen < 0 || headersLen > total-16 {
		return "", nil, fmt.Errorf("invalid kiro eventstream frame: total=%d headers=%d", total, headersLen)
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(d.r, rest); err != nil {
		return "", nil, err
	}
	checksum := crc32.NewIEEE()
	_, _ = checksum.Write(prelude)
	_, _ = checksum.Write(rest[:len(rest)-4])
	if checksum.Sum32() != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return "", nil, errors.New("kiro eventstream message CRC mismatch")
	}
	headers := rest[:headersLen]
	payload := append([]byte(nil), rest[headersLen:len(rest)-4]...)
	eventType := eventStreamHeader(headers, ":event-type")
	messageType := eventStreamHeader(headers, ":message-type")
	if exceptionType := eventStreamHeader(headers, ":exception-type"); exceptionType != "" {
		return "", nil, fmt.Errorf("kiro upstream exception %s", exceptionType)
	}
	if messageType == "exception" || messageType == "error" {
		return "", nil, errors.New("kiro upstream stream error")
	}
	return eventType, payload, nil
}

func eventStreamHeader(data []byte, target string) string {
	for pos := 0; pos < len(data); {
		nameLen := int(data[pos])
		pos++
		if pos+nameLen+1 > len(data) {
			return ""
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen
		typ := data[pos]
		pos++
		switch typ {
		case 0, 1:
			if name == target {
				if typ == 0 {
					return "true"
				}
				return "false"
			}
		case 2:
			pos++
		case 3:
			pos += 2
		case 4:
			pos += 4
		case 5, 8:
			pos += 8
		case 9:
			pos += 16
		case 6, 7:
			if pos+2 > len(data) {
				return ""
			}
			n := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			if pos+n > len(data) {
				return ""
			}
			value := string(data[pos : pos+n])
			pos += n
			if name == target {
				return value
			}
		default:
			return ""
		}
		if pos > len(data) {
			return ""
		}
	}
	return ""
}

type StreamParser struct {
	currentTool *toolState
	usage       Usage
}

type toolState struct{ id, name, input string }

func (p *StreamParser) Parse(eventType string, payload []byte) ([]Event, error) {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("decode kiro event %s: %w", eventType, err)
	}
	data := eventObject(root, eventType)
	switch eventType {
	case "assistantResponseEvent":
		if text := stringValue(data["content"]); text != "" {
			return []Event{{Type: "content", Content: text}}, nil
		}
	case "reasoningContentEvent":
		text, signature := stringValue(data["text"]), stringValue(data["signature"])
		if text != "" {
			p.usage.ReasoningTokens += estimateTokens(text)
		}
		return []Event{{Type: "reasoning", Reasoning: text, Signature: signature}}, nil
	case "toolUseEvent":
		return p.parseTool(data)
	case "messageMetadataEvent", "metadataEvent", "usageEvent", "usage":
		p.readUsage(data)
		u := p.usage
		return []Event{{Type: "usage", Usage: &u}}, nil
	}
	if _, ok := root["error"]; ok {
		return nil, errors.New("kiro upstream returned a stream error")
	}
	return nil, nil
}

func (p *StreamParser) Finish() []Event {
	if p.currentTool == nil {
		return nil
	}
	ev := p.finishTool()
	return []Event{ev}
}

func (p *StreamParser) Usage() Usage { return p.usage }

func (p *StreamParser) ObserveText(text string) { p.usage.OutputTokens += estimateTokens(text) }

func (p *StreamParser) parseTool(data map[string]any) ([]Event, error) {
	id, name := stringValue(data["toolUseId"]), stringValue(data["name"])
	var events []Event
	if id != "" && name != "" && (p.currentTool == nil || p.currentTool.id != id) {
		if p.currentTool != nil {
			events = append(events, p.finishTool())
		}
		p.currentTool = &toolState{id: id, name: name}
	}
	if p.currentTool == nil {
		return events, nil
	}
	switch input := data["input"].(type) {
	case string:
		p.currentTool.input += input
	case map[string]any:
		b, _ := json.Marshal(input)
		p.currentTool.input = string(b)
	}
	if stop, _ := data["stop"].(bool); stop {
		events = append(events, p.finishTool())
	}
	return events, nil
}

func (p *StreamParser) finishTool() Event {
	s := p.currentTool
	p.currentTool = nil
	input := map[string]any{}
	if s.input != "" {
		_ = json.Unmarshal([]byte(s.input), &input)
	}
	return Event{Type: "tool", ToolUse: &ToolUse{ToolUseID: s.id, Name: s.name, Input: input}}
}

func (p *StreamParser) readUsage(data map[string]any) {
	if nested, ok := data["tokenUsage"].(map[string]any); ok {
		data = nested
	}
	uncached := intValue(data["uncachedInputTokens"])
	read := intValue(data["cacheReadInputTokens"])
	write := intValue(data["cacheWriteInputTokens"])
	if total := uncached + read + write; total > 0 {
		p.usage.InputTokens = total
	}
	if v := intValue(data["inputTokens"]); v > 0 {
		p.usage.InputTokens = v
	}
	if v := intValue(data["outputTokens"]); v > 0 {
		p.usage.OutputTokens = v
	}
	p.usage.CacheReadTokens, p.usage.CacheWriteTokens = read, write
}

func eventObject(root map[string]any, eventType string) map[string]any {
	if v, ok := root[eventType].(map[string]any); ok {
		return v
	}
	return root
}
func stringValue(v any) string { s, _ := v.(string); return s }
func intValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	}
	return 0
}
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := (len([]rune(s)) + 2) / 3
	if n < 1 {
		return 1
	}
	return n
}
