package sipgox

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/emiago/sipgox/sdp"
	"github.com/pion/rtp"
)

// RTP Writer packetize any payload before pushing to active media session
type RTPReader struct {
	Sess *MediaSession

	// PacketHeader is stored after calling Read this will be stored before returning
	PacketHeader rtp.Header
	OnRTP        func(pkt *rtp.Packet)
	PayloadType  uint8
	Seq          RTPExtendedSequenceNumber

	unreadPayload []byte
	unread        int

	pktBuffer chan []byte

	// We want to track our last SSRC.
	lastSSRC uint32
}

// RTP reader consumes samples of audio from session
// TODO should it also decode ?
func NewRTPReader(sess *MediaSession) *RTPReader {
	f := sess.Formats[0]
	var payloadType uint8 = sdp.FormatNumeric(f)
	switch f {
	case sdp.FORMAT_TYPE_ALAW:
	case sdp.FORMAT_TYPE_ULAW:
		// TODO more support
	default:
		sess.log.Warn().Str("format", f).Msg("Unsupported format. Using default clock rate")
	}

	w := RTPReader{
		Sess:          sess,
		unreadPayload: []byte{},
		PayloadType:   payloadType,
		OnRTP:         func(pkt *rtp.Packet) {},

		pktBuffer: make(chan []byte, 100),
		Seq:       RTPExtendedSequenceNumber{},
	}

	return &w
}

// Read Implements io.Reader and extracts Payload from RTP packet
// has no input queue or sorting control of packets
// Buffer is used for reading headers and Headers are stored in PacketHeader
func (r *RTPReader) Read(b []byte) (int, error) {
	if r.unread > 0 {
		n := r.readPayload(b, r.unreadPayload)
		return n, nil
	}

	// Reuse read buffer.
	n, err := r.Sess.ReadRTPRaw(b)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return 0, io.EOF
		}

		return 0, err
	}
	pkt := rtp.Packet{}
	// NOTE: pkt after unmarshall will hold reference on b buffer.
	// Caller should do copy of PacketHeader if it reuses buffer
	if err := pkt.Unmarshal(b[:n]); err != nil {
		return 0, err
	}

	if r.PayloadType != pkt.PayloadType {
		return 0, fmt.Errorf("payload type does not match. expected=%d, actual=%d", r.PayloadType, pkt.PayloadType)
	}

	// If we are tracking this source, do check are we keep getting pkts in sequence
	if r.lastSSRC == pkt.SSRC {
		prevSeq := r.Seq.ReadExtendedSeq()
		if err := r.Seq.UpdateSeq(pkt.SequenceNumber); err != nil {
			r.Sess.log.Warn().Msg(err.Error())
		}

		newSeq := r.Seq.ReadExtendedSeq()
		if prevSeq+1 != newSeq {
			r.Sess.log.Warn().Uint64("expected", prevSeq+1).Uint64("actual", newSeq).Uint16("real", pkt.SequenceNumber).Msg("Out of order pkt received")
		}
	} else {
		r.Seq.InitSeq(pkt.SequenceNumber)
	}

	r.lastSSRC = pkt.SSRC
	r.PacketHeader = pkt.Header
	r.OnRTP(&pkt)

	return r.readPayload(b, pkt.Payload), nil
}

func (r *RTPReader) readPayload(b []byte, payload []byte) int {
	n := copy(b, payload)
	if n < len(payload) {
		r.unreadPayload = payload[n:]
		r.unread = len(payload) - n
	} else {
		r.unread = 0
	}
	return n
}
