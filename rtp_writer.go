package sipgox

import (
	"math/rand"
	"time"

	"github.com/emiago/sipgox/sdp"
	"github.com/pion/rtp"
)

// RTP Writer packetize any payload before pushing to active media session
// It creates SSRC as identifier and all packets sent will be with this SSRC
// For multiple streams, multiple RTP Writer needs to be created
type RTPWriter struct {
	Sess *MediaSession

	seq RTPExtendedSequenceNumber

	// Some defaults, can be overriten only after creating writer
	PayloadType        uint8
	SSRC               uint32
	SampleRate         uint32
	ClockRateTimestamp uint32
	clockTicker        *time.Ticker
	// MTU         uint32

	nextTimestamp uint32

	// After each write this is set as packet.
	LastPacket rtp.Packet
	OnRTP      func(pkt *rtp.Packet)
}

// RTP writer packetize payload in RTP packet before passing on media session
// Not having:
// - random SSRC
// - random Timestamp
// - allow different clock rate
// - CSRC contribution source
// - Silence detection and marker set
// - Padding and encryyption
func NewRTPWriter(sess *MediaSession) *RTPWriter {
	f := sess.Formats[0]
	var payloadType uint8 = sdp.FormatNumeric(f)
	var sampleRate uint32 = 8000
	clockRate := 20 * time.Millisecond
	switch f {
	case sdp.FORMAT_TYPE_ALAW:
	case sdp.FORMAT_TYPE_ULAW:
		// TODO more support
	default:
		sess.log.Warn().Str("format", f).Msg("Unsupported format. Using default clock rate")
	}

	w := RTPWriter{
		Sess:        sess,
		seq:         NewRTPSequencer(),
		PayloadType: payloadType,
		SampleRate:  sampleRate,
		SSRC:        rand.Uint32(),
		// MTU:         1500,

		// TODO: CSRC CSRC is contribution source identifiers.
		// This is set when media is passed trough mixer/translators and original SSRC wants to be preserverd
	}

	w.updateClockRate(clockRate)

	return &w
}

func (w *RTPWriter) updateClockRate(clockRate time.Duration) {
	w.ClockRateTimestamp = uint32(float64(w.SampleRate) * clockRate.Seconds())
	if w.clockTicker != nil {
		w.clockTicker.Stop()
	}
	w.clockTicker = time.NewTicker(clockRate)
}

// Write implements io.Writer and does payload RTP packetization
// Media clock rate is determined
// For more control or dynamic payload rate check WriteSamples
// It is not thread safe and order of payload frames is required
// Has no capabilities (yet):
// - MTU UDP limit handling
// - Media clock rate of payload is consistent
// - Packet loss detection
// - RTCP generating
func (p *RTPWriter) Write(b []byte) (int, error) {
	n, err := p.WriteSamples(b, p.ClockRateTimestamp, p.nextTimestamp == 0, p.PayloadType)
	<-p.clockTicker.C
	return n, err
}

func (p *RTPWriter) WriteSamples(payload []byte, clockRateTimestamp uint32, marker bool, payloadType uint8) (int, error) {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:     2,
			Padding:     false,
			Extension:   false,
			Marker:      marker,
			PayloadType: payloadType,
			// Timestamp should increase linear and monotonic for media clock
			// Payload must be in same clock rate
			// TODO: what about wrapp arround
			Timestamp:      p.nextTimestamp,
			SequenceNumber: p.seq.NextSeqNumber(),
			SSRC:           p.SSRC,
			CSRC:           []uint32{},
		},
		Payload: payload,
	}

	if p.OnRTP != nil {
		p.OnRTP(&pkt)
	}

	p.LastPacket = pkt
	p.nextTimestamp += clockRateTimestamp

	err := p.Sess.WriteRTP(&pkt)
	return len(pkt.Payload), err
}
