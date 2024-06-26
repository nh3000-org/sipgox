package sipgox

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgox/sdp"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	// RTPPortStart and RTPPortEnd allows defining rtp port range for media
	RTPPortStart  = 0
	RTPPortEnd    = 0
	rtpPortOffset = atomic.Int32{}

	RTPDebug  = false
	RTCPDebug = false
)

// MediaSession represents active media session with RTP/RTCP
type MediaSession struct {
	// Raddr is our target remote address. Normally it is resolved by SDP parsing.
	// Checkout SetRemoteAddr
	Raddr *net.UDPAddr
	// Laddr our local address which has full IP and port after media session creation
	Laddr *net.UDPAddr

	rtpConn   net.PacketConn
	rtcpConn  net.PacketConn
	rtcpRaddr *net.UDPAddr

	// SDP stuff
	// Depending of negotiation this can change.
	Formats sdp.Formats
	Mode    sdp.Mode

	log zerolog.Logger
}

func NewMediaSession(laddr *net.UDPAddr) (s *MediaSession, e error) {
	s = &MediaSession{
		Formats: sdp.Formats{
			sdp.FORMAT_TYPE_ULAW, sdp.FORMAT_TYPE_ALAW,
		},
		Laddr: laddr,
		Mode:  sdp.ModeSendrecv,
		log:   log.With().Str("caller", "media").Logger(),
	}

	// Try to listen on this ports
	if err := s.createListeners(s.Laddr); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *MediaSession) SetLogger(log zerolog.Logger) {
	s.log = log
}

// SetRemoteAddr is helper to set Raddr and rtcp address.
// It is not thread safe
func (s *MediaSession) SetRemoteAddr(raddr *net.UDPAddr) {
	s.Raddr = raddr
	s.rtcpRaddr = new(net.UDPAddr)
	*s.rtcpRaddr = *s.Raddr
	s.rtcpRaddr.Port++
}

func (s *MediaSession) LocalSDP() []byte {
	ip := s.Laddr.IP
	rtpPort := s.Laddr.Port

	return sdp.GenerateForAudio(ip, ip, rtpPort, s.Mode, s.Formats)
}

func (s *MediaSession) RemoteSDP(sdpReceived []byte) error {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(sdpReceived, &sd); err != nil {
		// p.log.Debug().Err(err).Msgf("Fail to parse SDP\n%q", string(r.Body()))
		return fmt.Errorf("fail to parse received SDP: %w", err)
	}

	md, err := sd.MediaDescription("audio")
	if err != nil {
		return err
	}

	ci, err := sd.ConnectionInformation()
	if err != nil {
		return err
	}

	raddr := &net.UDPAddr{IP: ci.IP, Port: md.Port}
	s.SetRemoteAddr(raddr)

	s.updateFormats(md.Formats)
	return nil
}

func (s *MediaSession) updateFormats(formats sdp.Formats) {
	// Check remote vs local
	if len(s.Formats) > 0 {
		filter := make([]string, 0, cap(formats))
		// Always prefer remote side?
		for _, cr := range formats {
			for _, cs := range s.Formats {
				if cr == cs {
					filter = append(filter, cr)
				}
			}
		}

		// for _, cs := range s.Formats {
		// 	for _, cr := range formats {
		// 		if cr == cs {
		// 			filter = append(filter, cr)
		// 		}
		// 	}
		// }
		// Update new list of formats
		s.Formats = sdp.Formats(filter)
	} else {
		s.Formats = formats
	}
}

// Listen creates listeners instead
func (s *MediaSession) createListeners(laddr *net.UDPAddr) error {
	// var err error

	if laddr.Port != 0 {
		return s.listenRTPandRTCP(laddr)
	}

	if laddr.Port == 0 && RTPPortStart > 0 && RTPPortEnd > RTPPortStart {
		// Get next available port
		port := RTPPortStart + int(rtpPortOffset.Load())
		var err error
		for ; port < RTPPortEnd; port += 2 {
			laddr.Port = port
			err = s.listenRTPandRTCP(laddr)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("No available ports in range %d:%d: %w", RTPPortStart, RTPPortEnd, err)
		}
		// Add some offset so that we use more from range
		offset := (port + 2 - RTPPortStart) % (RTPPortEnd - RTPPortStart)
		rtpPortOffset.Store(int32(offset)) // Reset to zero with module
		return nil
	}

	// Because we want to go +2 with ports in racy situations this will always fail
	// So we need to add some control and retry if needed
	// We are always in race with other services so only try to offset to reduce retries
	var err error
	for retries := 0; retries < 10; retries += 1 {
		err = s.listenRTPandRTCP(laddr)
		if err == nil {
			break
		}
	}

	return err
}

func (s *MediaSession) listenRTPandRTCP(laddr *net.UDPAddr) error {
	var err error
	s.rtpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port})
	if err != nil {
		return err
	}
	laddr = s.rtpConn.LocalAddr().(*net.UDPAddr)

	s.rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port + 1})
	if err != nil {
		s.rtpConn.Close()
		return err
	}

	// Update laddr as it can be empheral
	s.Laddr = laddr
	return nil
}

func (s *MediaSession) createListeners2(laddr *net.UDPAddr) error {
	var err error

	if laddr.Port == 0 && RTPPortStart > 0 && RTPPortEnd > RTPPortStart {
		// Get next available port
		port := RTPPortStart + int(rtpPortOffset.Load())
		for ; port < RTPPortEnd; port += 2 {
			rtpconn, err := net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: port})
			if err != nil {
				continue
			}
			rtpconn.Close()

			rtpcconn, err := net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: port + 1})
			if err != nil {
				continue
			}
			rtpcconn.Close()
			laddr.Port = port
			break
		}
		if laddr.Port == 0 {
			return fmt.Errorf("No available ports in range %d:%d", RTPPortStart, RTPPortEnd)
		}
		// Add some offset so that we use more from range
		offset := (port + 2 - RTPPortStart) % (RTPPortEnd - RTPPortStart)
		rtpPortOffset.Store(int32(offset)) // Reset to zero with module
	}

	// Without RTP port ranges this can fail on RTCP to be reused in racy conditions
	s.rtpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port})
	if err != nil {
		return err
	}
	laddr = s.rtpConn.LocalAddr().(*net.UDPAddr)

	s.rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port + 1})
	if err != nil {
		return err
	}

	// Update laddr as it can be empheral
	s.Laddr = laddr
	return nil
}

func (s *MediaSession) Close() {
	if s.rtcpConn != nil {
		s.rtcpConn.Close()
	}

	if s.rtpConn != nil {
		s.rtpConn.Close()
	}
}

func (s *MediaSession) UpdateDestinationSDP(sdpReceived []byte) error {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(sdpReceived, &sd); err != nil {
		// p.log.Debug().Err(err).Msgf("Fail to parse SDP\n%q", string(r.Body()))
		return fmt.Errorf("fail to parse received SDP: %w", err)
	}

	md, err := sd.MediaDescription("audio")
	if err != nil {
		return err
	}

	ci, err := sd.ConnectionInformation()
	if err != nil {
		return err
	}

	// TODO fix race problem, but it is rare this to happen
	s.Raddr.IP = ci.IP
	s.Raddr.Port = md.Port

	s.updateFormats(md.Formats)
	return nil
}

var rtpBufPool = &sync.Pool{
	New: func() any { return make([]byte, 1600) },
}

// readRTPNoAlloc will replace ReadRTP
// NOTE: this function will be replaced with passing packet as buf. This helps caller to reduce memory and GC
func (m *MediaSession) readRTPNoAlloc(pkt *rtp.Packet) error {
	buf := rtpBufPool.Get().([]byte)
	defer rtpBufPool.Put(buf)

	n, err := m.ReadRTPRaw(buf)
	if err != nil {
		return err
	}

	if err := rtpUnmarshal(buf[:n], pkt); err != nil {
		return err
	}

	// Problem is that this buffer is refferenced in rtp PACKET
	// if err := pkt.Unmarshal(buf[:n]); err != nil {
	// 	return err
	// }

	if RTPDebug {
		m.log.Debug().Msgf("Recv RTP\n%s", pkt.String())
	}
	return err
}

// Deprecated
// Will be replaced with readRTPNoAlloc in next releases
func (m *MediaSession) ReadRTP() (rtp.Packet, error) {
	p := rtp.Packet{}

	buf := make([]byte, 1600)

	n, err := m.ReadRTPRaw(buf)
	if err != nil {
		return p, err
	}

	if err := p.Unmarshal(buf[:n]); err != nil {
		return p, err
	}

	if RTPDebug {
		m.log.Debug().Msgf("Recv RTP\n%s", p.String())
	}
	return p, err
}

// Deprecated
// Will be replaced with readRTPDeadlineNoAlloc in next releases
func (m *MediaSession) ReadRTPDeadline(t time.Time) (rtp.Packet, error) {
	m.rtpConn.SetReadDeadline(t)
	return m.ReadRTP()
}

func (m *MediaSession) ReadRTPRaw(buf []byte) (int, error) {
	n, _, err := m.rtpConn.ReadFrom(buf)
	return n, err
}

func (m *MediaSession) ReadRTPRawDeadline(buf []byte, t time.Time) (int, error) {
	m.rtpConn.SetReadDeadline(t)
	return m.ReadRTPRaw(buf)
}

func (m *MediaSession) ReadRTCP(pkts []rtcp.Packet) (n int, err error) {
	// TODO fix this
	rawBuf := make([]byte, 1600)
	nn, err := m.ReadRTCPRaw(rawBuf)
	if err != nil {
		return n, err
	}

	n, err = rtcpUnmarshal(rawBuf[:nn], pkts)
	if err != nil {
		return 0, err
	}

	if RTCPDebug {
		for _, p := range pkts[:n] {
			if s, ok := p.(fmt.Stringer); ok {
				m.log.Debug().Msgf("RTCP read:\n%s", s.String())
				continue
			}
			log.Debug().Interface("data", p).
				Msg("RTCP read (Unknown):\n%s")
		}
	}
	return n, err
}

func (m *MediaSession) ReadRTCPDeadline(pkts []rtcp.Packet, t time.Time) (n int, err error) {
	m.rtcpConn.SetReadDeadline(t)
	return m.ReadRTCP(pkts)
}

func (m *MediaSession) ReadRTCPRaw(buf []byte) (int, error) {
	if m.rtcpConn == nil {
		// just block
		select {}
	}
	n, _, err := m.rtcpConn.ReadFrom(buf)

	return n, err
}

func (m *MediaSession) WriteRTP(p *rtp.Packet) error {
	if RTPDebug {
		m.log.Debug().Msgf("RTP write:\n%s", p.String())
	}

	data, err := p.Marshal()
	if err != nil {
		return err
	}

	n, err := m.WriteRTPRaw(data)
	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (m *MediaSession) WriteRTPRaw(data []byte) (n int, err error) {
	n, err = m.rtpConn.WriteTo(data, m.Raddr)
	return
}

func (m *MediaSession) WriteRTCP(p rtcp.Packet) error {
	if RTCPDebug {
		if sr, ok := p.(fmt.Stringer); ok {
			m.log.Debug().Msgf("RTCP write: \n%s", sr.String())
		}
	}

	data, err := p.Marshal()
	if err != nil {
		return err
	}

	return m.writeRTCP(data)
}

func (m *MediaSession) WriteRTCPDeadline(p rtcp.Packet, deadline time.Time) error {
	m.rtcpConn.SetWriteDeadline(deadline)
	return m.WriteRTCP(p)
}

// Use this to write Multi RTCP packets if they can fit in MTU=1500
func (m *MediaSession) WriteRTCPs(pkts []rtcp.Packet) error {
	data, err := rtcpMarshal(pkts)
	if err != nil {
		return err
	}

	return m.writeRTCP(data)
}

func (m *MediaSession) writeRTCP(data []byte) error {
	var err error
	var n int

	n, err = m.rtcpConn.WriteTo(data, m.rtcpRaddr)
	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func selectFormats(sendCodecs []string, recvCodecs []string) []int {
	formats := make([]int, 0, cap(sendCodecs))
	parseErr := []error{}
	for _, cr := range recvCodecs {
		for _, cs := range sendCodecs {
			if cr == cs {
				f, err := strconv.Atoi(cs)
				// keep going
				if err != nil {
					parseErr = append(parseErr, err)
					continue
				}
				formats = append(formats, f)
			}
		}
	}
	return formats
}

// DTMF event mapping (RFC 4733)
var dtmfEventMapping = map[rune]byte{
	'0': 0,
	'1': 1,
	'2': 2,
	'3': 3,
	'4': 4,
	'5': 5,
	'6': 6,
	'7': 7,
	'8': 8,
	'9': 9,
	'*': 10,
	'#': 11,
	'A': 12,
	'B': 13,
	'C': 14,
	'D': 15,
}

// RTPDTMFEncode creates series of DTMF redudant events which should be encoded as payload
// It is currently only 8000 sample rate considered for telophone event
func RTPDTMFEncode(char rune) []DTMFEvent {
	event := dtmfEventMapping[char]

	events := make([]DTMFEvent, 7)

	for i := 0; i < 4; i++ {
		d := DTMFEvent{
			Event:      event,
			EndOfEvent: false,
			Volume:     10,
			Duration:   160 * (uint16(i) + 1),
		}
		events[i] = d
	}

	// End events. Took this from linphone example, but not clear why sending this many
	for i := 4; i < 7; i++ {
		d := DTMFEvent{
			Event:      event,
			EndOfEvent: true,
			Volume:     10,
			Duration:   160 * 5, // Must not be increased for end event
		}
		events[i] = d
	}

	return events
}

// DTMFEvent represents a DTMF event
type DTMFEvent struct {
	Event      uint8
	EndOfEvent bool
	Volume     uint8
	Duration   uint16
}

func (ev *DTMFEvent) String() string {
	out := "RTP DTMF Event:\n"
	out += fmt.Sprintf("\tEvent: %d\n", ev.Event)
	out += fmt.Sprintf("\tEndOfEvent: %v\n", ev.EndOfEvent)
	out += fmt.Sprintf("\tVolume: %d\n", ev.Volume)
	out += fmt.Sprintf("\tDuration: %d\n", ev.Duration)
	return out
}

// DecodeRTPPayload decodes an RTP payload into a DTMF event
func DTMFDecode(payload []byte, d *DTMFEvent) error {
	if len(payload) < 4 {
		return fmt.Errorf("payload too short")
	}

	d.Event = payload[0]
	d.EndOfEvent = payload[1]&0x80 != 0
	d.Volume = payload[1] & 0x7F
	d.Duration = binary.BigEndian.Uint16(payload[2:4])
	// d.Duration = uint16(payload[2])<<8 | uint16(payload[3])
	return nil
}

func DTMFEncode(d DTMFEvent) []byte {
	header := make([]byte, 4)
	header[0] = d.Event

	if d.EndOfEvent {
		header[1] = 0x80
	}
	header[1] |= d.Volume & 0x3F
	binary.BigEndian.PutUint16(header[2:4], d.Duration)
	return header
}
