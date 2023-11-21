package sipgox

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type MediaSession struct {
	RTPport int
	Raddr   *net.UDPAddr
	Laddr   *net.UDPAddr

	rtpConn  net.PacketConn
	rtcpConn net.PacketConn

	rtpConnectedConn  net.Conn
	rtcpConnectedConn net.Conn

	Formats []int // For now can be set depending on SDP exchange
}

func NewMediaSessionFromSDP(sdpSend []byte, sdpReceived []byte) (s *MediaSession, err error) {
	sd := SessionDescription{}
	if err := UnmarshalSDP(sdpSend, &sd); err != nil {
		// p.log.Debug().Err(err).Msgf("Fail to parse SDP\n%q", string(r.Body()))
		return nil, fmt.Errorf("fail to parse send SDP: %w", err)
	}

	md, err := sd.MediaDescription("audio")
	if err != nil {
		return nil, err
	}

	ci, err := sd.ConnectionInformation()
	if err != nil {
		return nil, err
	}

	rtpPort := md.Port
	sendCodecs := md.Formats
	connectionIP := ci.IP

	sd = SessionDescription{}
	if err := UnmarshalSDP(sdpReceived, &sd); err != nil {
		// p.log.Debug().Err(err).Msgf("Fail to parse SDP\n%q", string(r.Body()))
		return nil, fmt.Errorf("fail to parse received SDP: %w", err)
	}

	md, err = sd.MediaDescription("audio")
	if err != nil {
		return nil, err
	}

	ci, err = sd.ConnectionInformation()
	if err != nil {
		return nil, err
	}

	dstIP := ci.IP
	dstPort := md.Port
	recvCodecs := md.Formats

	// Check codecs but expect all send codecs are send
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

	if len(formats) == 0 {
		emsg := "No formats found"
		for _, e := range parseErr {
			emsg += ": " + e.Error()
		}
		return nil, fmt.Errorf(emsg)
	}

	mess := NewMediaSession(
		formats,
		&net.UDPAddr{
			IP:   connectionIP,
			Port: rtpPort,
		},
		&net.UDPAddr{
			IP:   dstIP,
			Port: dstPort,
		},
	)
	return mess, nil
}

func NewMediaSession(formats []int, laddr *net.UDPAddr, raddr *net.UDPAddr) (s *MediaSession) {
	s = &MediaSession{
		Formats: formats,
		Laddr:   laddr,
		Raddr:   raddr,
	}

	return s
}

// Dial is setup connection for UDP, so it is more creating UPD listeners
func (s *MediaSession) Dial() error {
	laddr, raddr := s.Laddr, s.Raddr
	var err error

	dialerRTP := net.Dialer{
		LocalAddr: laddr,
	}

	dialerRTCP := net.Dialer{
		// RTCP is always rtpPort + 1
		LocalAddr: &net.UDPAddr{IP: laddr.IP, Port: laddr.Port + 1},
	}
	// RTP
	// rtpladdr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip.String(), rtpPort))
	s.rtpConnectedConn, err = dialerRTP.Dial("udp", raddr.String())
	if err != nil {
		return err
	}

	// s.rtpConn, err = net.ListenUDP("udp", rtpladdr)
	// if err != nil {
	// 	return nil, err
	// }

	// rtcpladdr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip.String(), rtpPort+1))
	// s.rtcpConn, err = net.ListenUDP("udp", rtcpladdr)
	dstAddr := net.JoinHostPort(raddr.IP.String(), strconv.Itoa(raddr.Port+1))
	// Check here is rtcp mux
	s.rtcpConnectedConn, err = dialerRTCP.Dial("udp", dstAddr)
	if err != nil {
		return err
	}

	return nil
}

// Listen creates listeners instead
func (s *MediaSession) Listen() error {
	laddr := s.Laddr
	var err error

	s.rtpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port})
	if err != nil {
		return err
	}

	s.rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: laddr.IP, Port: laddr.Port + 1})
	if err != nil {
		return err
	}

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
	sd := SessionDescription{}
	if err := UnmarshalSDP(sdpReceived, &sd); err != nil {
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

	s.Raddr.IP = ci.IP
	s.Raddr.Port = md.Port

	// TODO, we should maybe again check with our previous SDP?
	// we will consider that update is considering sent codecs
	s.Formats = selectFormats(md.Formats, md.Formats)
	return nil
}

func (m *MediaSession) ReadRTP() (rtp.Packet, error) {
	p := rtp.Packet{}

	buf := make([]byte, 1600)

	n, err := m.ReadRTPRaw(buf)
	if err != nil {
		return p, err
	}

	return p, p.Unmarshal(buf[:n])
}

func (m *MediaSession) ReadRTPWithDeadline(t time.Time) (rtp.Packet, error) {
	m.rtpConn.SetReadDeadline(t)
	return m.ReadRTP()
}

func (m *MediaSession) ReadRTPRaw(buf []byte) (int, error) {
	if m.rtpConnectedConn != nil {
		return m.rtpConnectedConn.Read(buf)
	}

	n, _, err := m.rtpConn.ReadFrom(buf)
	return n, err
}

func (m *MediaSession) ReadRTCP() ([]rtcp.Packet, error) {
	buf := make([]byte, 1600)

	n, err := m.ReadRTCPRaw(buf)
	if err != nil {
		return nil, err
	}

	return rtcp.Unmarshal(buf[:n])
}

func (m *MediaSession) ReadRTCPRaw(buf []byte) (int, error) {
	if m.rtcpConnectedConn != nil {
		return m.rtcpConnectedConn.Read(buf)
	}

	if m.rtcpConn == nil {
		// just block
		select {}
	}
	n, _, err := m.rtcpConn.ReadFrom(buf)

	return n, err
}

func (m *MediaSession) WriteRTP(p *rtp.Packet) error {
	data, err := p.Marshal()
	if err != nil {
		return err
	}

	var n int
	if m.rtpConnectedConn != nil {
		n, err = m.rtpConnectedConn.Write(data)
	} else {
		n, err = m.rtpConn.WriteTo(data, m.Raddr)
	}

	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (m *MediaSession) WriteRTCP(p rtcp.Packet) error {
	data, err := p.Marshal()
	if err != nil {
		return err
	}

	var n int
	if m.rtcpConnectedConn != nil {
		n, err = m.rtcpConnectedConn.Write(data)
	} else {
		n, err = m.rtcpConn.WriteTo(data, m.Raddr)
	}
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
