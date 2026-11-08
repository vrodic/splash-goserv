// Package wire is the server's side of the Splash native protocol
// (server/protocol.py, runtime/engine/Protocol.*): it encodes client frames
// and decodes engine events, little-endian and packed.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	ProtocolVersion     = 12
	FrameHeaderBytes    = 16
	StatusSchemaVersion = 8
	MaxThinkingEnd      = 64
	MaxImageSpans       = 64
	MaxFramePayload     = 256 * 1024 * 1024
	MaxStatusJSON       = 32 * 1024 * 1024
	MaxErrorString      = 1024 * 1024
	MaxPromptTokens     = 1 << 20
	MaxLogicalOutput    = 1 << 20
	MaxTokenBatch       = 4096
	MaxSimulation       = 32
	MaxScoreTokens      = 255
	RequestFixedBytes   = 114
	ImageSpanBytes      = 32
	noThinkingBudget    = 0xFFFFFFFF
)

var magic = [4]byte{'S', 'P', 'L', 'H'}

// Frame types.
const (
	FrameRequest        uint16 = 0x0001
	FrameCancel         uint16 = 0x0002
	FrameMaskResponse   uint16 = 0x0003
	FrameStatusRequest  uint16 = 0x0004
	FrameReleaseRequest uint16 = 0x0005
	FrameStop           uint16 = 0x0006

	FrameReady          uint16 = 0x0100
	FrameStart          uint16 = 0x0101
	FrameTokens         uint16 = 0x0102
	FrameMaskRequest    uint16 = 0x0103
	FrameDone           uint16 = 0x0104
	FrameError          uint16 = 0x0105
	FrameStatusJSON     uint16 = 0x0107
	FramePromptProgress uint16 = 0x0108
	FrameStartFailed    uint16 = 0x0109
	FrameWarming        uint16 = 0x010A
)

// Failure classes of an ErrorEvent.
const (
	RequestError    = 1
	EngineUnhealthy = 2
	ProtocolFatal   = 3
)

// Priorities.
const (
	PriorityForeground = 0
	PriorityNormal     = 1
	PriorityBackground = 2
)

// Request flags.
const (
	FlagIgnoreEOS      = 1 << 0
	FlagReturnProgress = 1 << 1
)

// Finish reasons.
const (
	FinishStop      = 0
	FinishLength    = 1
	FinishCancelled = 2
)

// Start failure kinds, by name for the serve record.
var StartFailureKinds = []string{"internal", "host_memory", "driver_allocation", "memory_budget",
	"context_budget", "configuration", "model", "device", "command_memory"}

// ShortfallBounds by name.
var ShortfallBounds = []string{"device_memory", "max_memory", "disk"}

// Sampling is the request's sampling block.
type Sampling struct {
	Temperature       float32
	TopP              float32
	TopK              uint32
	PresencePenalty   float32
	FrequencyPenalty  float32
	RepetitionPenalty float32
	MinP              float32
}

// ThinkingBudget caps the thinking a request generates in.
type ThinkingBudget struct {
	Tokens   uint32
	End      []uint32
	AlsoEnds uint32
}

// Request is one generation request.
type Request struct {
	ID                     uint64
	Priority               uint8
	Constraint             uint8
	AbsoluteDeadlineMicros uint64
	RemainingMicros        uint64
	MaxOutputTokens        uint32
	Prompt                 []uint32
	Sampling               Sampling
	Seed                   uint64
	GenerationPromptTokens uint32
	Flags                  uint32
	CacheSaltLo            uint64
	CacheSaltHi            uint64
	Thinking               *ThinkingBudget
}

func header(buf []byte, frameType uint16, payload int) {
	copy(buf[0:4], magic[:])
	binary.LittleEndian.PutUint16(buf[4:], ProtocolVersion)
	binary.LittleEndian.PutUint16(buf[6:], frameType)
	binary.LittleEndian.PutUint64(buf[8:], uint64(payload))
}

// Validate applies the request rules the engine enforces, so a bad request
// fails as a request error before it is written.
func (r *Request) Validate() error {
	if r.ID == 0 {
		return errors.New("request id must be non-zero")
	}
	if r.MaxOutputTokens == 0 || r.MaxOutputTokens > MaxLogicalOutput {
		return errors.New("logical max output token count exceeds its limit")
	}
	if len(r.Prompt) == 0 || len(r.Prompt) > MaxPromptTokens {
		return errors.New("prompt token count exceeds its limit")
	}
	if int(r.GenerationPromptTokens) >= len(r.Prompt) {
		return errors.New("generation prompt must leave a prompt token")
	}
	s := r.Sampling
	t := float64(s.Temperature)
	if !(t == 0 || (t >= 0x1p-126 && !math.IsInf(t, 0))) || !(s.TopP > 0 && s.TopP <= 1) || !(s.MinP >= 0 && s.MinP <= 1) {
		return errors.New("sampling requires temperature 0 or at least FLT_MIN, top_p in (0,1] and min_p in [0,1]")
	}
	if !(math.Abs(float64(s.PresencePenalty)) <= 2) || !(math.Abs(float64(s.FrequencyPenalty)) <= 2) ||
		!(s.RepetitionPenalty > 0) || math.IsInf(float64(s.RepetitionPenalty), 0) {
		return errors.New("sampling requires presence and frequency penalties in [-2,2] and a positive repetition penalty")
	}
	if r.Thinking != nil {
		end := r.Thinking.End
		if len(end) < 1 || len(end) > MaxThinkingEnd {
			return fmt.Errorf("thinking end token count must be in [1, %d]", MaxThinkingEnd)
		}
		last := end[len(end)-1]
		for _, t := range end[:len(end)-1] {
			if t == last || t == r.Thinking.AlsoEnds {
				return errors.New("only the last thinking end token may end thinking")
			}
		}
	}
	return nil
}

// Encode serializes the request frame, header included.
func (r *Request) Encode() []byte {
	var end []uint32
	if r.Thinking != nil {
		end = r.Thinking.End
	}
	payload := RequestFixedBytes + 4*len(r.Prompt) + 4*len(end)
	buf := make([]byte, FrameHeaderBytes+payload)
	header(buf, FrameRequest, payload)
	p := buf[FrameHeaderBytes:]
	le := binary.LittleEndian
	le.PutUint64(p[0:], r.ID)
	p[8] = r.Priority
	p[9] = r.Constraint
	le.PutUint64(p[10:], r.AbsoluteDeadlineMicros)
	le.PutUint64(p[18:], r.RemainingMicros)
	le.PutUint32(p[26:], r.MaxOutputTokens)
	le.PutUint32(p[30:], uint32(len(r.Prompt)))
	le.PutUint32(p[34:], 0) // image spans
	le.PutUint32(p[38:], math.Float32bits(r.Sampling.Temperature))
	le.PutUint32(p[42:], math.Float32bits(r.Sampling.TopP))
	le.PutUint32(p[46:], r.Sampling.TopK)
	le.PutUint32(p[50:], math.Float32bits(r.Sampling.PresencePenalty))
	le.PutUint32(p[54:], math.Float32bits(r.Sampling.FrequencyPenalty))
	le.PutUint32(p[58:], math.Float32bits(r.Sampling.RepetitionPenalty))
	le.PutUint32(p[62:], math.Float32bits(r.Sampling.MinP))
	le.PutUint64(p[66:], r.Seed)
	le.PutUint32(p[74:], 0) // score tokens
	le.PutUint32(p[78:], r.GenerationPromptTokens)
	le.PutUint32(p[82:], r.Flags)
	le.PutUint64(p[86:], r.CacheSaltLo)
	le.PutUint64(p[94:], r.CacheSaltHi)
	if r.Thinking == nil {
		le.PutUint32(p[102:], noThinkingBudget)
		le.PutUint32(p[106:], 0)
		le.PutUint32(p[110:], 0)
	} else {
		le.PutUint32(p[102:], r.Thinking.Tokens)
		le.PutUint32(p[106:], uint32(len(end)))
		le.PutUint32(p[110:], r.Thinking.AlsoEnds)
	}
	off := RequestFixedBytes
	for _, t := range r.Prompt {
		le.PutUint32(p[off:], t)
		off += 4
	}
	for _, t := range end {
		le.PutUint32(p[off:], t)
		off += 4
	}
	return buf
}

func idFrame(frameType uint16, id uint64) []byte {
	buf := make([]byte, FrameHeaderBytes+8)
	header(buf, frameType, 8)
	binary.LittleEndian.PutUint64(buf[FrameHeaderBytes:], id)
	return buf
}

// CancelFrame cancels a request.
func CancelFrame(id uint64) []byte { return idFrame(FrameCancel, id) }

// StopFrame ends a request at a client stop sequence.
func StopFrame(id uint64) []byte { return idFrame(FrameStop, id) }

// StatusRequestFrame asks for the status JSON.
func StatusRequestFrame(correlation uint64) []byte { return idFrame(FrameStatusRequest, correlation) }

// ReleaseRequestFrame asks the engine to give its weights back.
func ReleaseRequestFrame(correlation uint64) []byte {
	return idFrame(FrameReleaseRequest, correlation)
}

// Events.
type (
	Warming struct{}
	Ready   struct {
		MaxConcurrent uint32
		MaxContext    uint32
		Vision        bool
		Vocabulary    uint32
		MaskRows      uint32
		StopTokens    []uint32
	}
	Start struct {
		RequestID uint64
		Lane      uint32
		Matched   uint32
	}
	PromptProgress struct {
		RequestID     uint64
		Processed     uint32
		ElapsedMicros uint64
	}
	Tokens struct {
		RequestID uint64
		Offset    uint32
		Tokens    []uint32
	}
	MaskRequest struct {
		RequestID uint64
		MaskID    uint64
	}
	Done struct {
		RequestID        uint64
		Reason           uint8
		Bounded          bool
		PromptTokens     uint32
		CompletionTokens uint32
		PrefillMicros    uint64
		DecodeMicros     uint64
		WallMicros       uint64
		Deduplicated     uint32
		Drafted          uint32
		Accepted         uint32
		Cycles           uint32
	}
	ErrorEvent struct {
		Class      uint8
		Retryable  bool
		RetryAfter uint32
		RequestID  uint64
		Code       string
		Message    string
	}
	StatusJSON struct {
		Correlation uint64
		JSON        []byte
	}
	StartFailed struct {
		Kind        uint8
		Bound       uint8
		Required    uint64
		Budget      uint64
		PlanContext uint32
		Message     string
	}
)

// ProtocolError is a broken engine stream.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "protocol_fatal: " + e.Msg }

func fatal(format string, args ...any) error {
	return &ProtocolError{fmt.Sprintf(format, args...)}
}

// ReadEvent reads and decodes one engine event.
func ReadEvent(r io.Reader, scratch *[]byte) (any, error) {
	var h [FrameHeaderBytes]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, fatal("stream ended inside a frame header")
		}
		return nil, err
	}
	if [4]byte(h[0:4]) != magic {
		return nil, fatal("frame magic is not SPLH")
	}
	if v := binary.LittleEndian.Uint16(h[4:]); v != ProtocolVersion {
		return nil, fatal("unsupported native protocol version %d", v)
	}
	ft := binary.LittleEndian.Uint16(h[6:])
	n := binary.LittleEndian.Uint64(h[8:])
	if n > MaxFramePayload {
		return nil, fatal("payload length %d exceeds its safe limit", n)
	}
	if cap(*scratch) < int(n) {
		*scratch = make([]byte, n)
	}
	p := (*scratch)[:n]
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, fatal("stream ended inside a frame payload")
	}
	return decode(ft, p)
}

func words(p []byte, count int, label string) ([]uint32, error) {
	if len(p) != 4*count {
		return nil, fatal("%s count does not match the binary event payload", label)
	}
	out := make([]uint32, count)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(p[4*i:])
	}
	return out, nil
}

func need(p []byte, n int, name string) error {
	if len(p) < n {
		return fatal("%s payload length %d is below its required minimum %d", name, len(p), n)
	}
	return nil
}

func decode(ft uint16, p []byte) (any, error) {
	le := binary.LittleEndian
	switch ft {
	case FrameWarming:
		return Warming{}, nil
	case FrameReady:
		if err := need(p, 21, "ready"); err != nil {
			return nil, err
		}
		ev := Ready{MaxConcurrent: le.Uint32(p[0:]), MaxContext: le.Uint32(p[4:]), Vision: p[8] == 1,
			Vocabulary: le.Uint32(p[9:]), MaskRows: le.Uint32(p[13:])}
		stops, err := words(p[21:], int(le.Uint32(p[17:])), "stop token")
		if err != nil {
			return nil, err
		}
		ev.StopTokens = stops
		if p[8] > 1 || ev.MaxConcurrent == 0 || ev.MaxContext == 0 || ev.Vocabulary == 0 || len(stops) == 0 {
			return nil, fatal("ready event capacities and stop tokens must be non-zero")
		}
		return ev, nil
	case FrameStartFailed:
		if err := need(p, 26, "start_failed"); err != nil {
			return nil, err
		}
		msgLen := int(le.Uint32(p[22:]))
		if msgLen != len(p)-26 {
			return nil, fatal("start failure message length does not match the frame payload")
		}
		return StartFailed{Kind: p[0], Bound: p[1], Required: le.Uint64(p[2:]), Budget: le.Uint64(p[10:]),
			PlanContext: le.Uint32(p[18:]), Message: string(p[26:])}, nil
	case FrameStart:
		if len(p) != 16 {
			return nil, fatal("start payload length")
		}
		return Start{le.Uint64(p[0:]), le.Uint32(p[8:]), le.Uint32(p[12:])}, nil
	case FramePromptProgress:
		if len(p) != 20 {
			return nil, fatal("prompt progress payload length")
		}
		return PromptProgress{le.Uint64(p[0:]), le.Uint32(p[8:]), le.Uint64(p[12:])}, nil
	case FrameTokens:
		if err := need(p, 16, "tokens"); err != nil {
			return nil, err
		}
		toks, err := words(p[16:], int(le.Uint32(p[12:])), "token")
		if err != nil {
			return nil, err
		}
		if len(toks) == 0 {
			return nil, fatal("tokens event carries no token")
		}
		return Tokens{le.Uint64(p[0:]), le.Uint32(p[8:]), toks}, nil
	case FrameMaskRequest:
		if err := need(p, 24, "mask_request"); err != nil {
			return nil, err
		}
		return MaskRequest{le.Uint64(p[0:]), le.Uint64(p[8:])}, nil
	case FrameDone:
		if err := need(p, 62, "done"); err != nil {
			return nil, err
		}
		d := Done{RequestID: le.Uint64(p[0:]), Reason: p[8], Bounded: p[9] == 1,
			PromptTokens: le.Uint32(p[10:]), CompletionTokens: le.Uint32(p[14:]),
			PrefillMicros: le.Uint64(p[18:]), DecodeMicros: le.Uint64(p[26:]), WallMicros: le.Uint64(p[34:]),
			Deduplicated: le.Uint32(p[42:]), Drafted: le.Uint32(p[46:]), Accepted: le.Uint32(p[50:]),
			Cycles: le.Uint32(p[54:])}
		if _, err := words(p[62:], int(le.Uint32(p[58:])), "option logit"); err != nil {
			return nil, err
		}
		if d.Reason > FinishCancelled || p[9] > 1 || (d.Bounded && d.Reason != FinishLength) {
			return nil, fatal("done finish reason is invalid")
		}
		return d, nil
	case FrameError:
		if err := need(p, 22, "error"); err != nil {
			return nil, err
		}
		codeLen, msgLen := int(le.Uint32(p[14:])), int(le.Uint32(p[18:]))
		if codeLen+msgLen != len(p)-22 {
			return nil, fatal("error string lengths do not match the frame payload")
		}
		return ErrorEvent{Class: p[0], Retryable: p[1] == 1, RetryAfter: le.Uint32(p[2:]),
			RequestID: le.Uint64(p[6:]), Code: string(p[22 : 22+codeLen]), Message: string(p[22+codeLen:])}, nil
	case FrameStatusJSON:
		if err := need(p, 9, "status_json"); err != nil {
			return nil, err
		}
		return StatusJSON{le.Uint64(p[0:]), append([]byte(nil), p[8:]...)}, nil
	}
	return nil, fatal("frame type 0x%04x is not a native protocol event", ft)
}
