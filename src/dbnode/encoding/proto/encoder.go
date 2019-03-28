// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/m3db/m3/src/dbnode/encoding"
	"github.com/m3db/m3/src/dbnode/encoding/m3tsz"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/dbnode/x/xio"
	"github.com/m3db/m3x/checked"
	xtime "github.com/m3db/m3x/time"
	murmur3 "github.com/m3db/stackmurmur3"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
)

// Make sure encoder implements encoding.Encoder.
var _ encoding.Encoder = &Encoder{}

const (
	// Maximum capacity of a slice of TSZ fields that will be retained between resets.
	maxTSZFieldsCapacityRetain   = 24
	currentEncodingSchemeVersion = 1
)

var (
	encErrPrefix                         = "proto encoder:"
	errEncoderSchemaIsRequired           = fmt.Errorf("%s schema is required", encErrPrefix)
	errEncoderEncodingOptionsAreRequired = fmt.Errorf("%s encoding options are required", encErrPrefix)
	errEncoderMessageHasUnknownFields    = fmt.Errorf("%s message has unknown fields", encErrPrefix)
	errEncoderClosed                     = fmt.Errorf("%s encoder is closed", encErrPrefix)
	errNoEncodedDatapoints               = fmt.Errorf("%s encoder has no encoded datapoints", encErrPrefix)
)

// Encoder compresses arbitrary ProtoBuf streams given a schema.
// TODO(rartoul): Add support for changing the schema (and updating the ordering
// of the custom encoded fields) on demand: https://github.com/m3db/m3/issues/1471
type Encoder struct {
	opts encoding.Options

	stream encoding.OStream
	schema *desc.MessageDescriptor

	numEncoded    int
	lastEncodedDP ts.Datapoint
	lastEncoded   *dynamic.Message
	customFields  []customFieldState

	// Fields that are reused between function calls to
	// avoid allocations.
	varIntBuf              [8]byte
	changedValues          []int32
	fieldsChangedToDefault []int32
	unmarshaled            *dynamic.Message

	hasEncodedFirstSetOfCustomValues bool
	closed                           bool

	// We embed the m3tszEncoder (with a shared OStream) so that we can reuse
	// the delta-of-delta timestamp encoding logic. In the future, we could find
	// a nicer way to share this logic so that we don't have to store all of the
	// m3tszEncoder state that we don't need.
	m3tszEncoder *m3tsz.Encoder
}

// NewEncoder creates a new protobuf encoder.
func NewEncoder(start time.Time, opts encoding.Options) *Encoder {
	initAllocIfEmpty := opts.EncoderPool() == nil
	stream := encoding.NewOStream(nil, initAllocIfEmpty, opts.BytesPool())
	return &Encoder{
		opts:         opts,
		stream:       stream,
		m3tszEncoder: m3tsz.NewEncoder(start, nil, stream, false, opts).(*m3tsz.Encoder),
		varIntBuf:    [8]byte{},
	}
}

// Encode encodes a timestamp and a protobuf message. The function signature is strange
// in order to implement the encoding.Encoder interface. It accepts a ts.Datapoint, but
// only the Timestamp field will be used, the Value field will be ignored and will always
// return 0 on subsequent iteration. In addition, the provided annotation is expected to
// be a marshaled protobuf message that matches the configured schema.
func (enc *Encoder) Encode(dp ts.Datapoint, tu xtime.Unit, ant ts.Annotation) error {
	if enc.closed {
		return errEncoderClosed
	}
	if enc.schema == nil {
		return errEncoderSchemaIsRequired
	}

	if enc.unmarshaled == nil {
		// Lazy init.
		enc.unmarshaled = dynamic.NewMessage(enc.schema)
	}

	// Unmarshal the ProtoBuf message first to ensure we have a valid message before
	// we do anything else to reduce the change that we'll end up with a partially
	// encoded message.
	// TODO(rartoul): No need to alloate and unmarshal here, could do this in a streaming
	// fashion if we write our own decoder or expose the one in the underlying library.
	if err := enc.unmarshaled.Unmarshal(ant); err != nil {
		return fmt.Errorf(
			"%s error unmarshaling annotation into proto message: %v", encErrPrefix, err)
	}

	if enc.numEncoded == 0 {
		enc.encodeHeader()
	}

	// Control bit that indicates the stream has more data.
	enc.stream.WriteBit(opCodeMoreData)

	if err := enc.encodeTimestamp(dp.Timestamp, tu); err != nil {
		return fmt.Errorf(
			"%s error encoding timestamp: %v", encErrPrefix, err)
	}

	if err := enc.encodeProto(enc.unmarshaled); err != nil {
		return fmt.Errorf(
			"%s error encoding proto portion of message: %v", encErrPrefix, err)
	}

	enc.numEncoded++
	enc.lastEncodedDP = dp
	return nil
}

// Stream returns a copy of the underlying data stream.
func (enc *Encoder) Stream() xio.SegmentReader {
	seg := enc.segment(true)
	if seg.Len() == 0 {
		return nil
	}

	if readerPool := enc.opts.SegmentReaderPool(); readerPool != nil {
		reader := readerPool.Get()
		reader.Reset(seg)
		return reader
	}
	return xio.NewSegmentReader(seg)
}

func (enc *Encoder) segment(copy bool) ts.Segment {
	length := enc.stream.Len()
	if enc.stream.Len() == 0 {
		return ts.Segment{}
	}

	var head checked.Bytes
	buffer, _ := enc.stream.Rawbytes()
	if !copy {
		// Take ref from the ostream.
		head = enc.stream.Discard()
	} else {
		// Copy into new buffer.
		head = enc.newBuffer(length)
		head.IncRef()
		head.AppendAll(buffer)
		head.DecRef()
	}

	return ts.NewSegment(head, nil, ts.FinalizeHead)
}

// NumEncoded returns the number of encoded messages.
func (enc *Encoder) NumEncoded() int {
	return enc.numEncoded
}

// LastEncoded returns the last encoded datapoint. Does not include
// annotation / protobuf message for interface purposes.
func (enc *Encoder) LastEncoded() (ts.Datapoint, error) {
	if enc.numEncoded == 0 {
		return ts.Datapoint{}, errNoEncodedDatapoints
	}

	return enc.lastEncodedDP, nil
}

// Len returns the length of the data stream.
func (enc *Encoder) Len() int {
	return enc.stream.Len()
}

func (enc *Encoder) encodeHeader() {
	enc.encodeVarInt(currentEncodingSchemeVersion)
	enc.encodeVarInt(uint64(enc.opts.ByteFieldDictionaryLRUSize()))
	enc.encodeCustomSchemaTypes()
}

func (enc *Encoder) encodeCustomSchemaTypes() {
	if len(enc.customFields) == 0 {
		enc.encodeVarInt(0)
		return
	}

	// Field numbers are 1-indexed so encoding the maximum field number
	// at the beginning is equivalent to encoding the number of types
	// we need to read after if we imagine that we're encoding a 1-indexed
	// bitset where the position in the bitset encodes the field number (I.E
	// the first value is the type for field number 1) and the values are
	// the number of bits required to unique identify a custom type instead of
	// just being a single bit (3 bits in the case of version 1 of the encoding
	// scheme.)
	maxFieldNum := enc.customFields[len(enc.customFields)-1].fieldNum
	enc.encodeVarInt(uint64(maxFieldNum))

	// Start at 1 because we're zero-indexed.
	for i := 1; i <= maxFieldNum; i++ {
		customTypeBits := uint64(cNotCustomEncoded)
		for _, customField := range enc.customFields {
			if customField.fieldNum == i {
				customTypeBits = uint64(customField.fieldType)
				break
			}
		}

		enc.stream.WriteBits(
			customTypeBits,
			numBitsToEncodeCustomType)
	}
}

func (enc *Encoder) encodeTimestamp(t time.Time, tu xtime.Unit) error {
	if !enc.hasEncodedFirstSetOfCustomValues {
		return enc.m3tszEncoder.WriteFirstTime(t, nil, tu)
	}
	return enc.m3tszEncoder.WriteNextTime(t, nil, tu)
}

// TODO: Add concept of hard/soft error and if there is a hard error
// then the encoder cant be used anymore.
func (enc *Encoder) encodeProto(m *dynamic.Message) error {
	if len(m.GetUnknownFields()) > 0 {
		// TODO(rartoul): Make this behavior configurable.
		return errEncoderMessageHasUnknownFields
	}

	if err := enc.encodeCustomValues(m); err != nil {
		return err
	}
	if err := enc.encodeProtoValues(m); err != nil {
		return err
	}

	return nil
}

// Reset resets the encoder for reuse.
func (enc *Encoder) Reset(
	start time.Time,
	capacity int,
) {
	enc.reset(start, capacity)
}

// SetSchema sets the encoders schema.
// TODO(rartoul): Add support for changing the schema (and updating the ordering
// of the custom encoded fields) on demand: https://github.com/m3db/m3/issues/1471
func (enc *Encoder) SetSchema(schema *desc.MessageDescriptor) {
	enc.resetSchema(schema)
}

func (enc *Encoder) reset(start time.Time, capacity int) {
	// Resetting the m3tsz encoder will take care of resetting the shared ostream
	// so we don't need to do that again in this function.
	enc.m3tszEncoder.Reset(start, capacity)
	enc.lastEncoded = nil
	enc.lastEncodedDP = ts.Datapoint{}
	enc.unmarshaled = nil

	if enc.schema != nil {
		enc.customFields = resetCustomFields(enc.customFields, enc.schema)
	}

	enc.hasEncodedFirstSetOfCustomValues = false
	enc.closed = false
	enc.numEncoded = 0
}

func (enc *Encoder) resetSchema(schema *desc.MessageDescriptor) {
	enc.schema = schema
	enc.customFields = resetCustomFields(enc.customFields, enc.schema)

	enc.lastEncoded = dynamic.NewMessage(schema)
	enc.unmarshaled = dynamic.NewMessage(schema)
}

// Close closes the encoder.
func (enc *Encoder) Close() {
	if enc.closed {
		return
	}

	enc.closed = true
	enc.Reset(time.Time{}, 0)
	enc.stream.Reset(nil)
	enc.m3tszEncoder.Reset(time.Time{}, 0)

	if pool := enc.opts.EncoderPool(); pool != nil {
		pool.Put(enc)
	}
}

// Discard closes the encoder and transfers ownership of the data stream to
// the caller.
func (enc *Encoder) Discard() ts.Segment {
	segment := enc.discard()
	// Close the encoder since its no longer needed
	enc.Close()
	return segment
}

// DiscardReset does the same thing as Discard except it also resets the encoder
// for reuse.
func (enc *Encoder) DiscardReset(start time.Time, capacity int) ts.Segment {
	segment := enc.discard()
	enc.Reset(start, capacity)
	return segment
}

func (enc *Encoder) discard() ts.Segment {
	return enc.segment(false)
}

// Bytes returns the raw bytes of the underlying data stream. Does not
// transfer ownership and is generally unsafe.
func (enc *Encoder) Bytes() ([]byte, error) {
	if enc.closed {
		return nil, errEncoderClosed
	}

	bytes, _ := enc.stream.Rawbytes()
	return bytes, nil
}

func (enc *Encoder) encodeCustomValues(m *dynamic.Message) error {
	for i, customField := range enc.customFields {
		iVal, err := m.TryGetFieldByNumber(customField.fieldNum)
		if err != nil {
			return fmt.Errorf(
				"%s error trying to get field number: %d",
				encErrPrefix, customField.fieldNum)
		}

		customEncoded := true
		switch {
		case isCustomFloatEncodedField(customField.fieldType):
			if err := enc.encodeTSZValue(i, iVal); err != nil {
				return err
			}
		case isCustomIntEncodedField(customField.fieldType):
			if err := enc.encodeIntValue(i, iVal); err != nil {
				return err
			}
		case customField.fieldType == cBytes:
			if err := enc.encodeBytesValue(i, iVal); err != nil {
				return err
			}
		default:
			customEncoded = false
		}

		if customEncoded {
			// Remove the field from the message so we don't include it
			// in the proto marshal.
			m.ClearFieldByNumber(customField.fieldNum)
		}
	}
	enc.hasEncodedFirstSetOfCustomValues = true

	return nil
}

func (enc *Encoder) encodeTSZValue(i int, iVal interface{}) error {
	var (
		val         float64
		customField = enc.customFields[i]
	)
	switch typedVal := iVal.(type) {
	case float64:
		val = typedVal
	case float32:
		val = float64(typedVal)
	default:
		return fmt.Errorf(
			"%s found unknown type in fieldNum %d", encErrPrefix, customField.fieldNum)
	}

	if !enc.hasEncodedFirstSetOfCustomValues {
		enc.encodeFirstTSZValue(i, val)
	} else {
		enc.encodeNextTSZValue(i, val)
	}

	return nil
}

func (enc *Encoder) encodeIntValue(i int, iVal interface{}) error {
	var (
		signedVal   int64
		unsignedVal uint64
		customField = enc.customFields[i]
	)
	switch typedVal := iVal.(type) {
	case uint64:
		unsignedVal = typedVal
	case uint32:
		unsignedVal = uint64(typedVal)
	case int64:
		signedVal = typedVal
	case int32:
		signedVal = int64(typedVal)
	default:
		return fmt.Errorf(
			"%s found unknown type in fieldNum %d", encErrPrefix, customField.fieldNum)
	}

	if isUnsignedInt(customField.fieldType) {
		if !enc.hasEncodedFirstSetOfCustomValues {
			enc.encodeFirstUnsignedIntValue(i, unsignedVal)
		} else {
			enc.encodeNextUnsignedIntValue(i, unsignedVal)
		}
	} else {
		if !enc.hasEncodedFirstSetOfCustomValues {
			enc.encodeFirstSignedIntValue(i, signedVal)
		} else {
			enc.encodeNextSignedIntValue(i, signedVal)
		}
	}

	return nil
}

func (enc *Encoder) encodeBytesValue(i int, iVal interface{}) error {
	customField := enc.customFields[i]
	currBytes, ok := iVal.([]byte)
	if !ok {
		currString, ok := iVal.(string)
		if !ok {
			return fmt.Errorf(
				"%s found unknown type in fieldNum %d", encErrPrefix, customField.fieldNum)
		}
		currBytes = []byte(currString)
	}

	var (
		hash             = murmur3.Sum64(currBytes)
		numPreviousBytes = len(customField.bytesFieldDict)
		lastStateIdx     = numPreviousBytes - 1
		lastState        encoderBytesFieldDictState
	)
	if numPreviousBytes > 0 {
		lastState = customField.bytesFieldDict[lastStateIdx]
	}

	if numPreviousBytes > 0 && hash == lastState.hash {
		streamBytes, _ := enc.stream.Rawbytes()
		match, err := enc.bytesMatchEncodedDictionaryValue(
			streamBytes, lastState, currBytes)
		if err != nil {
			return fmt.Errorf(
				"%s error checking if bytes match last encoded dictionary bytes: %v",
				encErrPrefix, err)
		}
		if match {
			// No changes control bit.
			enc.stream.WriteBit(opCodeNoChange)
			return nil
		}
	}

	// Bytes changed control bit.
	enc.stream.WriteBit(opCodeChange)

	streamBytes, _ := enc.stream.Rawbytes()
	for j, state := range customField.bytesFieldDict {
		if hash != state.hash {
			continue
		}

		match, err := enc.bytesMatchEncodedDictionaryValue(
			streamBytes, state, currBytes)
		if err != nil {
			return fmt.Errorf(
				"%s error checking if bytes match encoded dictionary bytes: %v",
				encErrPrefix, err)
		}
		if !match {
			continue
		}

		// Control bit means interpret next n bits as the index for the previous write
		// that this matches where n is the number of bits required to represent all
		// possible array indices in the configured LRU size.
		enc.stream.WriteBit(opCodeInterpretSubsequentBitsAsLRUIndex)
		enc.stream.WriteBits(
			uint64(j),
			numBitsRequiredForNumUpToN(
				enc.opts.ByteFieldDictionaryLRUSize()))
		enc.moveToEndOfBytesDict(i, j)
		return nil
	}

	// Control bit means interpret subsequent bits as varInt encoding length of a new
	// []byte we haven't seen before.
	enc.stream.WriteBit(opCodeInterpretSubsequentBitsAsBytesLengthVarInt)

	length := len(currBytes)
	enc.encodeVarInt((uint64(length)))

	// Add padding bits until we reach the next byte. This ensures that the startPos
	// that we're going to store in the dictionary LRU will be aligned on a physical
	// byte boundary which makes retrieving the bytes again later for comparison much
	// easier.
	//
	// Note that this will waste up to a maximum of 7 bits per []byte that we encode
	// which is acceptable for now, but in the future we may want to make the code able
	// to do the comparison even if the bytes aren't aligned on a byte boundary in order
	// to improve the compression.
	enc.padToNextByte()

	// Track the byte position we're going to start at so we can store it in the LRU after.
	streamBytes, _ = enc.stream.Rawbytes()
	bytePos := len(streamBytes)

	// Write the actual bytes.
	enc.stream.WriteBytes(currBytes)

	enc.addToBytesDict(i, encoderBytesFieldDictState{
		hash:     hash,
		startPos: bytePos,
		length:   length,
	})
	return nil
}

func (enc *Encoder) encodeProtoValues(m *dynamic.Message) error {
	// Reset for re-use.
	enc.changedValues = enc.changedValues[:0]
	changedFields := enc.changedValues

	enc.fieldsChangedToDefault = enc.fieldsChangedToDefault[:0]
	fieldsChangedToDefault := enc.fieldsChangedToDefault

	if enc.lastEncoded != nil {
		schemaFields := enc.schema.GetFields()
		for _, field := range m.GetKnownFields() {
			// Clear out any fields that were provided but are not in the schema
			// to prevent wasting space on them.
			// TODO(rartoul):This is an uncommon scenario and this operation might
			// be expensive to perform each time so consider removing this if it
			// impacts performance too much.
			fieldNum := field.GetNumber()
			if !fieldsContains(fieldNum, schemaFields) {
				if err := m.TryClearFieldByNumber(int(fieldNum)); err != nil {
					return fmt.Errorf(
						"error: %v clearing field that does not exist in schema: %d", err, fieldNum)
				}
			}
		}

		for _, field := range schemaFields {
			var (
				fieldNum    = field.GetNumber()
				fieldNumInt = int(fieldNum)
				prevVal     = enc.lastEncoded.GetFieldByNumber(fieldNumInt)
				curVal      = m.GetFieldByNumber(fieldNumInt)
			)

			if fieldsEqual(curVal, prevVal) {
				// Clear fields that haven't changed.
				if err := m.TryClearFieldByNumber(fieldNumInt); err != nil {
					return fmt.Errorf("error: %v clearing field: %d", err, fieldNumInt)
				}
			} else {
				isDefaultValue, err := isDefaultValue(field, curVal)
				if err != nil {
					return fmt.Errorf(
						"error: %v, checking if %v is default value for field %s",
						err, curVal, field.String())
				}
				if isDefaultValue {
					fieldsChangedToDefault = append(fieldsChangedToDefault, fieldNum)
				}

				changedFields = append(changedFields, fieldNum)
				if err := enc.lastEncoded.TrySetFieldByNumber(fieldNumInt, curVal); err != nil {
					return fmt.Errorf(
						"error: %v setting field %d with value %v on lastEncoded",
						err, fieldNumInt, curVal)
				}
			}
		}
	}

	if len(changedFields) == 0 && enc.lastEncoded != nil {
		// Only want to skip encoding if nothing has changed AND we've already
		// encoded the first message.
		enc.stream.WriteBit(opCodeNoChange)
		return nil
	}

	// TODO(rartoul): Need to add a MarshalInto to the ProtoReflect library to save
	// allocations: https://github.com/m3db/m3/issues/1471
	marshaled, err := m.Marshal()
	if err != nil {
		return fmt.Errorf("%s error trying to marshal protobuf: %v", encErrPrefix, err)
	}

	// Control bit indicating that proto values have changed.
	enc.stream.WriteBit(opCodeChange)
	if len(fieldsChangedToDefault) > 0 {
		// Control bit indicating that some fields have been set to default values
		// and that a bitset will follow specifying which fields have changed.
		enc.stream.WriteBit(opCodeFieldsSetToDefaultProtoMarshal)
		enc.encodeBitset(fieldsChangedToDefault)
	} else {
		// Control bit indicating that none of the changed fields have been set to
		// their default values so we can do a clean merge on read.
		enc.stream.WriteBit(opCodeNoFieldsSetToDefaultProtoMarshal)
	}
	enc.encodeVarInt(uint64(len(marshaled)))
	enc.stream.WriteBytes(marshaled)

	if enc.lastEncoded == nil {
		// Set lastEncoded to m so that subsequent encodings only need to encode fields
		// that have changed.
		enc.lastEncoded = dynamic.NewMessage(enc.schema)
		enc.lastEncoded.Merge(m)
	} else {
		// lastEncoded has already been mutated to reflect the current state.
	}

	return nil
}

func (enc *Encoder) encodeFirstTSZValue(i int, v float64) {
	fb := math.Float64bits(v)
	enc.stream.WriteBits(fb, 64)
	enc.customFields[i].prevFloatBits = fb
	enc.customFields[i].prevXOR = fb
}

func (enc *Encoder) encodeNextTSZValue(i int, next float64) {
	curFloatBits := math.Float64bits(next)
	curXOR := enc.customFields[i].prevFloatBits ^ curFloatBits
	m3tsz.WriteXOR(enc.stream, enc.customFields[i].prevXOR, curXOR)
	enc.customFields[i].prevFloatBits = curFloatBits
	enc.customFields[i].prevXOR = curXOR
}

func (enc *Encoder) encodeFirstSignedIntValue(i int, v int64) {
	neg := false
	enc.customFields[i].prevFloatBits = uint64(v)
	if v < 0 {
		neg = true
		v = -1 * v
	}

	vBits := uint64(v)
	numSig := encoding.NumSig(vBits)

	m3tsz.WriteIntSig(enc.stream, &enc.customFields[i].intSigBitsTracker, numSig)
	enc.encodeIntValDiff(vBits, neg, numSig)
}

func (enc *Encoder) encodeFirstUnsignedIntValue(i int, v uint64) {
	enc.customFields[i].prevFloatBits = uint64(v)

	vBits := uint64(v)
	numSig := encoding.NumSig(vBits)

	m3tsz.WriteIntSig(enc.stream, &enc.customFields[i].intSigBitsTracker, numSig)
	enc.encodeIntValDiff(vBits, false, numSig)
}

func (enc *Encoder) encodeNextSignedIntValue(i int, next int64) {
	prev := int64(enc.customFields[i].prevFloatBits)
	diff := next - prev
	if diff == 0 {
		enc.stream.WriteBit(opCodeNoChange)
		return
	}

	enc.stream.WriteBit(opCodeChange)

	neg := false
	if diff < 0 {
		neg = true
		diff = -1 * diff
	}

	var (
		diffBits = uint64(diff)
		numSig   = encoding.NumSig(diffBits)
		newSig   = enc.customFields[i].intSigBitsTracker.TrackNewSig(numSig)
	)
	// Can't use an intermediary variable for the intSigBitsTracker because we need to
	// modify the value in the slice, not a copy of it.
	m3tsz.WriteIntSig(enc.stream, &enc.customFields[i].intSigBitsTracker, newSig)
	enc.encodeIntValDiff(diffBits, neg, newSig)
	enc.customFields[i].prevFloatBits = uint64(next)
}

func (enc *Encoder) encodeNextUnsignedIntValue(i int, next uint64) {
	var (
		neg  = false
		prev = uint64(enc.customFields[i].prevFloatBits)
		diff uint64
	)

	// Avoid overflows.
	if next > prev {
		diff = next - prev
	} else {
		neg = true
		diff = prev - next
	}

	if diff == 0 {
		enc.stream.WriteBit(opCodeNoChange)
		return
	}

	enc.stream.WriteBit(opCodeChange)

	numSig := encoding.NumSig(diff)
	newSig := enc.customFields[i].intSigBitsTracker.TrackNewSig(numSig)
	// Can't use an intermediary variable for the intSigBitsTracker because we need to
	// modify the value in the slice, not a copy of it.
	m3tsz.WriteIntSig(enc.stream, &enc.customFields[i].intSigBitsTracker, newSig)
	enc.encodeIntValDiff(diff, neg, newSig)
	enc.customFields[i].prevFloatBits = uint64(next)
}

func (enc *Encoder) encodeIntValDiff(valBits uint64, neg bool, numSig uint8) {
	if neg {
		// opCodeNegative
		enc.stream.WriteBit(opCodeIntDeltaNegative)
	} else {
		// opCodePositive
		enc.stream.WriteBit(opCodeIntDeltaPositive)
	}

	enc.stream.WriteBits(valBits, int(numSig))
}

func (enc *Encoder) bytesMatchEncodedDictionaryValue(
	streamBytes []byte,
	dictState encoderBytesFieldDictState,
	currBytes []byte,
) (bool, error) {
	var (
		prevEncodedBytesStart = dictState.startPos
		prevEncodedBytesEnd   = prevEncodedBytesStart + dictState.length
	)

	if prevEncodedBytesEnd > len(streamBytes) {
		// Should never happen.
		return false, fmt.Errorf(
			"bytes position in LRU is outside of stream bounds, streamSize: %d, startPos: %d, length: %d",
			len(streamBytes), prevEncodedBytesStart, dictState.length)
	}

	return bytes.Equal(streamBytes[prevEncodedBytesStart:prevEncodedBytesEnd], currBytes), nil
}

// padToNextByte will add padding bits in the current byte until the ostream
// reaches the beginning of the next byte. This allows us begin encoding data
// with the guarantee that we're aligned at a physical byte boundary.
func (enc *Encoder) padToNextByte() {
	_, bitPos := enc.stream.Rawbytes()
	for bitPos%8 != 0 {
		enc.stream.WriteBit(0)
		bitPos++
	}
}

func (enc *Encoder) moveToEndOfBytesDict(fieldIdx, i int) {
	existing := enc.customFields[fieldIdx].bytesFieldDict
	for j := i; j < len(existing); j++ {
		nextIdx := j + 1
		if nextIdx >= len(existing) {
			break
		}

		currVal := existing[j]
		nextVal := existing[nextIdx]
		existing[j] = nextVal
		existing[nextIdx] = currVal
	}
}

func (enc *Encoder) addToBytesDict(fieldIdx int, state encoderBytesFieldDictState) {
	existing := enc.customFields[fieldIdx].bytesFieldDict
	if len(existing) < enc.opts.ByteFieldDictionaryLRUSize() {
		enc.customFields[fieldIdx].bytesFieldDict = append(existing, state)
		return
	}

	// Shift everything down 1 and replace the last value to evict the
	// least recently used entry and add the newest one.
	//     [1,2,3]
	// becomes
	//     [2,3,3]
	// after shift, and then becomes
	//     [2,3,4]
	// after replacing the last value.
	for i := range existing {
		nextIdx := i + 1
		if nextIdx >= len(existing) {
			break
		}

		existing[i] = existing[nextIdx]
	}

	existing[len(existing)-1] = state
}

// encodeBitset writes out a bitset in the form of:
//
//      varint(number of bits)|bitset
//
// I.E first it encodes a varint which specifies the number of following
// bits to interpret as a bitset and then it encodes the provided values
// as zero-indexed bitset.
func (enc *Encoder) encodeBitset(values []int32) {
	var max int32
	for _, v := range values {
		if v > max {
			max = v
		}
	}

	// Encode a varint that indicates how many of the remaining
	// bits to interpret as a bitset.
	enc.encodeVarInt(uint64(max))

	// Encode the bitset
	for i := int32(0); i < max; i++ {
		wroteExists := false

		for _, v := range values {
			// Subtract one because the values are 1-indexed but the bitset
			// is 0-indexed.
			if i == v-1 {
				enc.stream.WriteBit(opCodeBitsetValueIsSet)
				wroteExists = true
				break
			}
		}

		if wroteExists {
			continue
		}

		enc.stream.WriteBit(opCodeBitsetValueIsNotSet)
	}
}

func (enc *Encoder) encodeVarInt(x uint64) {
	var (
		// Convert array to slice we can reuse the buffer.
		buf      = enc.varIntBuf[:]
		numBytes = binary.PutUvarint(buf, x)
	)

	// Reslice so we only write out as many bytes as is required
	// to represent the number.
	buf = buf[:numBytes]
	enc.stream.WriteBytes(buf)
}

func (enc *Encoder) newBuffer(capacity int) checked.Bytes {
	if bytesPool := enc.opts.BytesPool(); bytesPool != nil {
		return bytesPool.Get(capacity)
	}
	return checked.NewBytes(make([]byte, 0, capacity), nil)
}
