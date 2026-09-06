package otlp

import (
	"encoding/binary"
	"math"
)

// A minimal protobuf wire-format writer.
//
// OTLP's default encoding is protobuf, and every collector accepts it while
// only some accept OTLP/JSON — so the gateway speaks it, and speaking it here
// costs less than the dependency would. Encoding one fixed, known schema needs
// only the four wire types below; there is no reflection, no descriptor
// registry and no decoder, because nothing here ever reads a protobuf back.
//
// The schema this serialises is opentelemetry-proto's metrics service. Field
// numbers are part of the wire contract and are written as constants at their
// point of use, next to the message they belong to, so a reader can check one
// against the .proto without holding the whole schema in mind.

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// buf accumulates encoded bytes.
type buf struct{ b []byte }

func (e *buf) tag(field, wire int) {
	e.uvarint(uint64(field)<<3 | uint64(wire))
}

func (e *buf) uvarint(v uint64) {
	for v >= 0x80 {
		e.b = append(e.b, byte(v)|0x80)
		v >>= 7
	}
	e.b = append(e.b, byte(v))
}

// str writes a length-delimited string, skipping the field entirely when empty:
// proto3 treats an absent scalar and a zero one as the same value, so writing
// empties would only inflate the payload.
func (e *buf) str(field int, s string) {
	if s == "" {
		return
	}
	e.tag(field, wireBytes)
	e.uvarint(uint64(len(s)))
	e.b = append(e.b, s...)
}

func (e *buf) fixed64(field int, v uint64) {
	if v == 0 {
		return
	}
	e.tag(field, wireFixed64)
	e.b = binary.LittleEndian.AppendUint64(e.b, v)
}

// double writes a double. Unlike the other scalars it is written even when
// zero, because a zero measurement is a fact — a cost of exactly nothing, a
// gauge at rest — and dropping it would leave the consumer to invent one.
func (e *buf) double(field int, f float64) {
	e.tag(field, wireFixed64)
	e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(f))
}

func (e *buf) varint(field int, v uint64) {
	if v == 0 {
		return
	}
	e.tag(field, wireVarint)
	e.uvarint(v)
}

func (e *buf) boolean(field int, v bool) {
	if !v {
		return
	}
	e.tag(field, wireVarint)
	e.uvarint(1)
}

// packedFixed64 writes a repeated fixed64 field in its packed form, which is
// the proto3 default for repeated scalars.
func (e *buf) packedFixed64(field int, vs []uint64) {
	if len(vs) == 0 {
		return
	}
	e.tag(field, wireBytes)
	e.uvarint(uint64(len(vs) * 8))
	for _, v := range vs {
		e.b = binary.LittleEndian.AppendUint64(e.b, v)
	}
}

// packedDouble writes a repeated double field in its packed form.
func (e *buf) packedDouble(field int, vs []float64) {
	if len(vs) == 0 {
		return
	}
	e.tag(field, wireBytes)
	e.uvarint(uint64(len(vs) * 8))
	for _, v := range vs {
		e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(v))
	}
}

// message writes a nested message. The submessage is encoded first because its
// length has to precede it on the wire, which is also why this cannot stream.
// The payloads here are a few tens of kilobytes at most, once an interval.
func (e *buf) message(field int, fn func(*buf)) {
	var inner buf
	fn(&inner)
	e.tag(field, wireBytes)
	e.uvarint(uint64(len(inner.b)))
	e.b = append(e.b, inner.b...)
}
