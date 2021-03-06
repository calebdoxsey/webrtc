// Package webrtc implements the WebRTC 1.0 as defined in W3C WebRTC specification document.
package webrtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"encoding/binary"

	"github.com/pions/webrtc/internal/network"
	"github.com/pions/webrtc/internal/sdp"
	"github.com/pions/webrtc/pkg/ice"
	"github.com/pions/webrtc/pkg/media"
	"github.com/pions/webrtc/pkg/rtcerr"
	"github.com/pions/webrtc/pkg/rtcp"
	"github.com/pions/webrtc/pkg/rtp"
	"github.com/pkg/errors"
)

// Unknown defines default public constant to use for "enum" like struct
// comparisons when no value was defined.
const Unknown = iota

// RTCPeerConnection represents a WebRTC connection that establishes a
// peer-to-peer communications with another RTCPeerConnection instance in a
// browser, or to another endpoint implementing the required protocols.
type RTCPeerConnection struct {
	sync.RWMutex

	configuration RTCConfiguration

	// CurrentLocalDescription represents the local description that was
	// successfully negotiated the last time the RTCPeerConnection transitioned
	// into the stable state plus any local candidates that have been generated
	// by the IceAgent since the offer or answer was created.
	CurrentLocalDescription *RTCSessionDescription

	// PendingLocalDescription represents a local description that is in the
	// process of being negotiated plus any local candidates that have been
	// generated by the IceAgent since the offer or answer was created. If the
	// RTCPeerConnection is in the stable state, the value is null.
	PendingLocalDescription *RTCSessionDescription

	// CurrentRemoteDescription represents the last remote description that was
	// successfully negotiated the last time the RTCPeerConnection transitioned
	// into the stable state plus any remote candidates that have been supplied
	// via AddIceCandidate() since the offer or answer was created.
	CurrentRemoteDescription *RTCSessionDescription

	// PendingRemoteDescription represents a remote description that is in the
	// process of being negotiated, complete with any remote candidates that
	// have been supplied via AddIceCandidate() since the offer or answer was
	// created. If the RTCPeerConnection is in the stable state, the value is
	// null.
	PendingRemoteDescription *RTCSessionDescription

	// SignalingState attribute returns the signaling state of the
	// RTCPeerConnection instance.
	SignalingState RTCSignalingState

	// IceGatheringState attribute returns the ICE gathering state of the
	// RTCPeerConnection instance.
	IceGatheringState RTCIceGatheringState // FIXME NOT-USED

	// IceConnectionState attribute returns the ICE connection state of the
	// RTCPeerConnection instance.
	// IceConnectionState RTCIceConnectionState  // FIXME SWAP-FOR-THIS
	IceConnectionState ice.ConnectionState // FIXME REMOVE

	// ConnectionState attribute returns the connection state of the
	// RTCPeerConnection instance.
	ConnectionState RTCPeerConnectionState

	idpLoginURL *string

	isClosed          bool
	negotiationNeeded bool

	lastOffer  string
	lastAnswer string

	// Media
	mediaEngine     *MediaEngine
	rtpTransceivers []*RTCRtpTransceiver

	// sctpTransport
	sctpTransport *RTCSctpTransport

	// DataChannels
	dataChannels map[uint16]*RTCDataChannel

	// OnNegotiationNeeded        func() // FIXME NOT-USED
	// OnIceCandidate             func() // FIXME NOT-USED
	// OnIceCandidateError        func() // FIXME NOT-USED
	// OnSignalingStateChange     func() // FIXME NOT-USED

	// OnIceConnectionStateChange designates an event handler which is called
	// when an ice connection state is changed.
	OnICEConnectionStateChange func(ice.ConnectionState)

	// OnIceGatheringStateChange  func() // FIXME NOT-USED
	// OnConnectionStateChange    func() // FIXME NOT-USED

	// OnTrack designates an event handler which is called when remote track
	// arrives from a remote peer.
	OnTrack func(*RTCTrack)

	// OnDataChannel designates an event handler which is invoked when a data
	// channel message arrives from a remote peer.
	OnDataChannel func(*RTCDataChannel)

	// Deprecated: Internal mechanism which will be removed.
	networkManager *network.Manager

	backgroundActions chan func()
}

// New creates a new RTCPeerConfiguration with the provided configuration
func New(configuration RTCConfiguration) (*RTCPeerConnection, error) {
	// https://w3c.github.io/webrtc-pc/#constructor (Step #2)
	// Some variables defined explicitly despite their implicit zero values to
	// allow better readability to understand what is happening.
	pc := RTCPeerConnection{
		configuration: RTCConfiguration{
			IceServers:           []RTCIceServer{},
			IceTransportPolicy:   RTCIceTransportPolicyAll,
			BundlePolicy:         RTCBundlePolicyBalanced,
			RtcpMuxPolicy:        RTCRtcpMuxPolicyRequire,
			Certificates:         []RTCCertificate{},
			IceCandidatePoolSize: 0,
		},
		isClosed:          false,
		negotiationNeeded: false,
		lastOffer:         "",
		lastAnswer:        "",
		SignalingState:    RTCSignalingStateStable,
		// IceConnectionState: RTCIceConnectionStateNew, // FIXME SWAP-FOR-THIS
		IceConnectionState: ice.ConnectionStateNew, // FIXME REMOVE
		IceGatheringState:  RTCIceGatheringStateNew,
		ConnectionState:    RTCPeerConnectionStateNew,
		mediaEngine:        DefaultMediaEngine,
		sctpTransport:      newRTCSctpTransport(),
		dataChannels:       make(map[uint16]*RTCDataChannel),
		backgroundActions:  make(chan func(), 1),
	}

	var err error
	if err = pc.initConfiguration(configuration); err != nil {
		return nil, err
	}

	pc.networkManager, err = network.NewManager(pc.generateChannel, pc.dataChannelEventHandler, pc.iceStateChange)
	if err != nil {
		return nil, err
	}

	// FIXME Temporary code before IceAgent and RTCIceTransport Rebuild
	for _, server := range pc.configuration.IceServers {
		for _, rawURL := range server.URLs {
			url, err := ice.ParseURL(rawURL)
			if err != nil {
				return nil, err
			}

			err = pc.networkManager.AddURL(url)
			if err != nil {
				fmt.Println(err)
			}
		}
	}

	go func() {
		for action := range pc.backgroundActions {
			action()
		}
	}()

	return &pc, nil
}

// initConfiguration defines validation of the specified RTCConfiguration and
// its assignment to the internal configuration variable. This function differs
// from its SetConfiguration counterpart because most of the checks do not
// include verification statements related to the existing state. Thus the
// function describes only minor verification of some the struct variables.
func (pc *RTCPeerConnection) initConfiguration(configuration RTCConfiguration) error {
	if configuration.PeerIdentity != "" {
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#constructor (step #3)
	if len(configuration.Certificates) > 0 {
		now := time.Now()
		for _, x509Cert := range configuration.Certificates {
			if !x509Cert.Expires().IsZero() && now.After(x509Cert.Expires()) {
				return &rtcerr.InvalidAccessError{Err: ErrCertificateExpired}
			}
			pc.configuration.Certificates = append(pc.configuration.Certificates, x509Cert)
		}
	} else {
		sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return &rtcerr.UnknownError{Err: err}
		}
		certificate, err := GenerateCertificate(sk)
		if err != nil {
			return err
		}
		pc.configuration.Certificates = []RTCCertificate{*certificate}
	}

	if configuration.BundlePolicy != RTCBundlePolicy(Unknown) {
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	if configuration.RtcpMuxPolicy != RTCRtcpMuxPolicy(Unknown) {
		pc.configuration.RtcpMuxPolicy = configuration.RtcpMuxPolicy
	}

	if configuration.IceCandidatePoolSize != 0 {
		pc.configuration.IceCandidatePoolSize = configuration.IceCandidatePoolSize
	}

	if configuration.IceTransportPolicy != RTCIceTransportPolicy(Unknown) {
		pc.configuration.IceTransportPolicy = configuration.IceTransportPolicy
	}

	if len(configuration.IceServers) > 0 {
		for _, server := range configuration.IceServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.IceServers = configuration.IceServers
	}
	return nil
}

// SetConfiguration updates the configuration of this RTCPeerConnection object.
func (pc *RTCPeerConnection) SetConfiguration(configuration RTCConfiguration) error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-setconfiguration (step #2)
	if pc.isClosed {
		return &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #3)
	if configuration.PeerIdentity != "" {
		if configuration.PeerIdentity != pc.configuration.PeerIdentity {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingPeerIdentity}
		}
		pc.configuration.PeerIdentity = configuration.PeerIdentity
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #4)
	if len(configuration.Certificates) > 0 {
		if len(configuration.Certificates) != len(pc.configuration.Certificates) {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
		}

		for i, certificate := range configuration.Certificates {
			if !pc.configuration.Certificates[i].Equals(certificate) {
				return &rtcerr.InvalidModificationError{Err: ErrModifyingCertificates}
			}
		}
		pc.configuration.Certificates = configuration.Certificates
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #5)
	if configuration.BundlePolicy != RTCBundlePolicy(Unknown) {
		if configuration.BundlePolicy != pc.configuration.BundlePolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingBundlePolicy}
		}
		pc.configuration.BundlePolicy = configuration.BundlePolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #6)
	if configuration.RtcpMuxPolicy != RTCRtcpMuxPolicy(Unknown) {
		if configuration.RtcpMuxPolicy != pc.configuration.RtcpMuxPolicy {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingRtcpMuxPolicy}
		}
		pc.configuration.RtcpMuxPolicy = configuration.RtcpMuxPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #7)
	if configuration.IceCandidatePoolSize != 0 {
		if pc.configuration.IceCandidatePoolSize != configuration.IceCandidatePoolSize &&
			pc.LocalDescription() != nil {
			return &rtcerr.InvalidModificationError{Err: ErrModifyingIceCandidatePoolSize}
		}
		pc.configuration.IceCandidatePoolSize = configuration.IceCandidatePoolSize
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #8)
	if configuration.IceTransportPolicy != RTCIceTransportPolicy(Unknown) {
		pc.configuration.IceTransportPolicy = configuration.IceTransportPolicy
	}

	// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11)
	if len(configuration.IceServers) > 0 {
		// https://www.w3.org/TR/webrtc/#set-the-configuration (step #11.3)
		for _, server := range configuration.IceServers {
			if err := server.validate(); err != nil {
				return err
			}
		}
		pc.configuration.IceServers = configuration.IceServers
	}
	return nil
}

// GetConfiguration returns an RTCConfiguration object representing the current
// configuration of this RTCPeerConnection object. The returned object is a
// copy and direct mutation on it will not take affect until SetConfiguration
// has been called with RTCConfiguration passed as its only argument.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-getconfiguration
func (pc *RTCPeerConnection) GetConfiguration() RTCConfiguration {
	return pc.configuration
}

// ------------------------------------------------------------------------
// --- FIXME - BELOW CODE NEEDS REVIEW/CLEANUP
// ------------------------------------------------------------------------

// CreateOffer starts the RTCPeerConnection and generates the localDescription
func (pc *RTCPeerConnection) CreateOffer(options *RTCOfferOptions) (RTCSessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	if options != nil {
		return RTCSessionDescription{}, errors.Errorf("TODO handle options")
	} else if useIdentity {
		return RTCSessionDescription{}, errors.Errorf("TODO handle identity provider")
	} else if pc.isClosed {
		return RTCSessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	d := sdp.NewJSEPSessionDescription(pc.networkManager.DTLSFingerprint(), useIdentity)
	candidates := pc.generateLocalCandidates()

	bundleValue := "BUNDLE"

	if pc.addRTPMediaSection(d, RTCRtpCodecTypeAudio, "audio", RTCRtpTransceiverDirectionSendrecv, candidates, sdp.ConnectionRoleActpass) {
		bundleValue += " audio"
	}
	if pc.addRTPMediaSection(d, RTCRtpCodecTypeVideo, "video", RTCRtpTransceiverDirectionSendrecv, candidates, sdp.ConnectionRoleActpass) {
		bundleValue += " video"
	}

	pc.addDataMediaSection(d, "data", candidates, sdp.ConnectionRoleActpass)
	d = d.WithValueAttribute(sdp.AttrKeyGroup, bundleValue+" data")

	for _, m := range d.MediaDescriptions {
		m.WithPropertyAttribute("setup:actpass")
	}

	pc.CurrentLocalDescription = &RTCSessionDescription{
		Type:   RTCSdpTypeOffer,
		Sdp:    d.Marshal(),
		parsed: d,
	}

	return *pc.CurrentLocalDescription, nil
}

// CreateAnswer starts the RTCPeerConnection and generates the localDescription
func (pc *RTCPeerConnection) CreateAnswer(options *RTCAnswerOptions) (RTCSessionDescription, error) {
	useIdentity := pc.idpLoginURL != nil
	if options != nil {
		return RTCSessionDescription{}, errors.Errorf("TODO handle options")
	} else if useIdentity {
		return RTCSessionDescription{}, errors.Errorf("TODO handle identity provider")
	} else if pc.isClosed {
		return RTCSessionDescription{}, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	candidates := pc.generateLocalCandidates()
	d := sdp.NewJSEPSessionDescription(pc.networkManager.DTLSFingerprint(), useIdentity)

	bundleValue := "BUNDLE"
	for _, remoteMedia := range pc.CurrentRemoteDescription.parsed.MediaDescriptions {
		// TODO @trivigy better SDP parser
		var peerDirection RTCRtpTransceiverDirection
		midValue := ""
		for _, a := range remoteMedia.Attributes {
			if strings.HasPrefix(*a.String(), "mid") {
				midValue = (*a.String())[len("mid:"):]
			} else if strings.HasPrefix(*a.String(), "sendrecv") {
				peerDirection = RTCRtpTransceiverDirectionSendrecv
			} else if strings.HasPrefix(*a.String(), "sendonly") {
				peerDirection = RTCRtpTransceiverDirectionSendonly
			} else if strings.HasPrefix(*a.String(), "recvonly") {
				peerDirection = RTCRtpTransceiverDirectionRecvonly
			}
		}

		appendBundle := func() {
			bundleValue += " " + midValue
		}

		if strings.HasPrefix(*remoteMedia.MediaName.String(), "audio") {
			if pc.addRTPMediaSection(d, RTCRtpCodecTypeAudio, midValue, peerDirection, candidates, sdp.ConnectionRoleActive) {
				appendBundle()
			}
		} else if strings.HasPrefix(*remoteMedia.MediaName.String(), "video") {
			if pc.addRTPMediaSection(d, RTCRtpCodecTypeVideo, midValue, peerDirection, candidates, sdp.ConnectionRoleActive) {
				appendBundle()
			}
		} else if strings.HasPrefix(*remoteMedia.MediaName.String(), "application") {
			pc.addDataMediaSection(d, midValue, candidates, sdp.ConnectionRoleActive)
			appendBundle()
		}
	}

	d = d.WithValueAttribute(sdp.AttrKeyGroup, bundleValue)

	pc.CurrentLocalDescription = &RTCSessionDescription{
		Type:   RTCSdpTypeAnswer,
		Sdp:    d.Marshal(),
		parsed: d,
	}
	return *pc.CurrentLocalDescription, nil
}

// // SetLocalDescription sets the SessionDescription of the local peer
// func (pc *RTCPeerConnection) SetLocalDescription() {
// 	panic("not implemented yet") // FIXME NOT-IMPLEMENTED nolint
// }

// LocalDescription returns PendingLocalDescription if it is not null and
// otherwise it returns CurrentLocalDescription. This property is used to
// determine if setLocalDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-localdescription
func (pc *RTCPeerConnection) LocalDescription() *RTCSessionDescription {
	if pc.PendingLocalDescription != nil {
		return pc.PendingLocalDescription
	}
	return pc.CurrentLocalDescription
}

// SetRemoteDescription sets the SessionDescription of the remote peer
func (pc *RTCPeerConnection) SetRemoteDescription(desc RTCSessionDescription) error {
	if pc.CurrentRemoteDescription != nil {
		return errors.Errorf("remoteDescription is already defined, SetRemoteDescription can only be called once")
	}

	weOffer := true
	remoteUfrag := ""
	remotePwd := ""
	if desc.Type == RTCSdpTypeOffer {
		weOffer = false
	}

	pc.CurrentRemoteDescription = &desc
	pc.CurrentRemoteDescription.parsed = &sdp.SessionDescription{}
	if err := pc.CurrentRemoteDescription.parsed.Unmarshal(pc.CurrentRemoteDescription.Sdp); err != nil {
		return err
	}

	for _, m := range pc.CurrentRemoteDescription.parsed.MediaDescriptions {
		for _, a := range m.Attributes {
			if strings.HasPrefix(*a.String(), "candidate") {
				if c := sdp.ICECandidateUnmarshal(*a.String()); c != nil {
					pc.networkManager.IceAgent.AddRemoteCandidate(c)
				} else {
					fmt.Printf("Tried to parse ICE candidate, but failed %s ", a)
				}
			} else if strings.HasPrefix(*a.String(), "ice-ufrag") {
				remoteUfrag = (*a.String())[len("ice-ufrag:"):]
			} else if strings.HasPrefix(*a.String(), "ice-pwd") {
				remotePwd = (*a.String())[len("ice-pwd:"):]
			}
		}
	}
	return pc.networkManager.Start(weOffer, remoteUfrag, remotePwd)
}

// RemoteDescription returns PendingRemoteDescription if it is not null and
// otherwise it returns CurrentRemoteDescription. This property is used to
// determine if setRemoteDescription has already been called.
// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-remotedescription
func (pc *RTCPeerConnection) RemoteDescription() *RTCSessionDescription {
	if pc.PendingRemoteDescription != nil {
		return pc.PendingRemoteDescription
	}
	return pc.CurrentRemoteDescription
}

// AddIceCandidate accepts an ICE candidate string and adds it
// to the existing set of candidates
func (pc *RTCPeerConnection) AddIceCandidate(s string) error {
	if c := sdp.ICECandidateUnmarshal(s); c != nil {
		pc.networkManager.IceAgent.AddRemoteCandidate(c)
		return nil
	}
	return fmt.Errorf("Unable to parse %q as remote candidate", s)
}

// ------------------------------------------------------------------------
// --- FIXME - BELOW CODE NEEDS RE-ORGANIZATION - https://w3c.github.io/webrtc-pc/#rtp-media-api
// ------------------------------------------------------------------------

// GetSenders returns the RTCRtpSender that are currently attached to this RTCPeerConnection
func (pc *RTCPeerConnection) GetSenders() []RTCRtpSender {
	result := make([]RTCRtpSender, len(pc.rtpTransceivers))
	for i, tranceiver := range pc.rtpTransceivers {
		result[i] = *tranceiver.Sender
	}
	return result
}

// GetReceivers returns the RTCRtpReceivers that are currently attached to this RTCPeerConnection
func (pc *RTCPeerConnection) GetReceivers() []RTCRtpReceiver {
	result := make([]RTCRtpReceiver, len(pc.rtpTransceivers))
	for i, tranceiver := range pc.rtpTransceivers {
		result[i] = *tranceiver.Receiver
	}
	return result
}

// GetTransceivers returns the RTCRtpTransceiver that are currently attached to this RTCPeerConnection
func (pc *RTCPeerConnection) GetTransceivers() []RTCRtpTransceiver {
	result := make([]RTCRtpTransceiver, len(pc.rtpTransceivers))
	for i, tranceiver := range pc.rtpTransceivers {
		result[i] = *tranceiver
	}
	return result
}

// AddTrack adds a RTCTrack to the RTCPeerConnection
func (pc *RTCPeerConnection) AddTrack(track *RTCTrack) (*RTCRtpSender, error) {
	if pc.isClosed {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}
	for _, transceiver := range pc.rtpTransceivers {
		if transceiver.Sender.Track == nil {
			continue
		}
		if track.ID == transceiver.Sender.Track.ID {
			return nil, &rtcerr.InvalidAccessError{Err: ErrExistingTrack}
		}
	}
	var transceiver *RTCRtpTransceiver
	for _, t := range pc.rtpTransceivers {
		if !t.stopped &&
			// t.Sender == nil && // TODO: check that the sender has never sent
			t.Sender.Track == nil &&
			t.Receiver.Track != nil &&
			t.Receiver.Track.Kind == track.Kind {
			transceiver = t
			break
		}
	}
	if transceiver != nil {
		if err := transceiver.setSendingTrack(track); err != nil {
			return nil, err
		}
	} else {
		var receiver *RTCRtpReceiver
		sender := newRTCRtpSender(track)
		transceiver = pc.newRTCRtpTransceiver(
			receiver,
			sender,
			RTCRtpTransceiverDirectionSendonly,
		)
	}

	transceiver.Mid = track.Kind.String() // TODO: Mid generation

	return transceiver.Sender, nil
}

// func (pc *RTCPeerConnection) RemoveTrack() {
// 	panic("not implemented yet") // FIXME NOT-IMPLEMENTED nolint
// }

// func (pc *RTCPeerConnection) AddTransceiver() RTCRtpTransceiver {
// 	panic("not implemented yet") // FIXME NOT-IMPLEMENTED nolint
// }

// ------------------------------------------------------------------------
// --- FIXME - BELOW CODE NEEDS RE-ORGANIZATION - https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api
// ------------------------------------------------------------------------

// CreateDataChannel creates a new RTCDataChannel object with the given label
// and optitional RTCDataChannelInit used to configure properties of the
// underlying channel such as data reliability.
func (pc *RTCPeerConnection) CreateDataChannel(label string, options *RTCDataChannelInit) (*RTCDataChannel, error) {
	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #2)
	if pc.isClosed {
		return nil, &rtcerr.InvalidStateError{Err: ErrConnectionClosed}
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #5)
	if len(label) > 65535 {
		return nil, &rtcerr.TypeError{Err: ErrStringSizeLimit}
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #3)
	// Some variables defined explicitly despite their implicit zero values to
	// allow better readability to understand what is happening. Additionally,
	// some members are set to a non zero value default due to the default
	// definitions in https://w3c.github.io/webrtc-pc/#dom-rtcdatachannelinit
	// which are later overwriten by the options if any were specified.
	channel := RTCDataChannel{
		rtcPeerConnection: pc,
		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #4)
		Label:             label,
		Ordered:           true,
		MaxPacketLifeTime: nil,
		MaxRetransmits:    nil,
		Protocol:          "",
		Negotiated:        false,
		ID:                nil,
		Priority:          RTCPriorityTypeLow,
		// https://w3c.github.io/webrtc-pc/#dfn-create-an-rtcdatachannel (Step #2)
		ReadyState: RTCDataChannelStateConnecting,
		// https://w3c.github.io/webrtc-pc/#dfn-create-an-rtcdatachannel (Step #3)
		BufferedAmount: 0,
	}

	if options != nil {
		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #7)
		if options.MaxPacketLifeTime != nil {
			channel.MaxPacketLifeTime = options.MaxPacketLifeTime
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #8)
		if options.MaxRetransmits != nil {
			channel.MaxRetransmits = options.MaxRetransmits
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #9)
		if options.Ordered != nil {
			channel.Ordered = *options.Ordered
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #10)
		if options.Protocol != nil {
			channel.Protocol = *options.Protocol
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #12)
		if options.Negotiated != nil {
			channel.Negotiated = *options.Negotiated
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #13)
		if options.ID != nil && channel.Negotiated {
			channel.ID = options.ID
		}

		// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #15)
		if options.Priority != nil {
			channel.Priority = *options.Priority
		}
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #11)
	if len(channel.Protocol) > 65535 {
		return nil, &rtcerr.TypeError{Err: ErrStringSizeLimit}
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #14)
	if channel.Negotiated && channel.ID == nil {
		return nil, &rtcerr.TypeError{Err: ErrNegotiatedWithoutID}
	}

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #16)
	if channel.MaxPacketLifeTime != nil && channel.MaxRetransmits != nil {
		return nil, &rtcerr.TypeError{Err: ErrRetransmitsOrPacketLifeTime}
	}

	// FIXME https://w3c.github.io/webrtc-pc/#dom-rtcpeerconnection-createdatachannel (Step #17)

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #20)
	channel.Transport = pc.sctpTransport

	// https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #19)
	if channel.ID == nil {
		var err error
		if channel.ID, err = pc.generateDataChannelID(true); err != nil {
			return nil, err
		}
		// if err := channel.generateID(); err != nil {
		// 	return nil, err
		// }
	}

	// // https://w3c.github.io/webrtc-pc/#peer-to-peer-data-api (Step #18)
	if *channel.ID > 65534 {
		return nil, &rtcerr.TypeError{Err: ErrMaxDataChannelID}
	}

	if pc.sctpTransport.State == RTCSctpTransportStateConnected &&
		*channel.ID >= *pc.sctpTransport.MaxChannels {
		return nil, &rtcerr.OperationError{Err: ErrMaxDataChannelID}
	}

	// Remember datachannel
	pc.dataChannels[*channel.ID] = &channel

	// Send opening message
	// pc.networkManager.SendOpenChannelMessage(id, label)

	return &channel, nil
}

func (pc *RTCPeerConnection) generateDataChannelID(client bool) (*uint16, error) {
	var id uint16
	if !client {
		id++
	}

	for ; id < *pc.sctpTransport.MaxChannels-1; id += 2 {
		_, ok := pc.dataChannels[id]
		if !ok {
			return &id, nil
		}
	}
	return nil, &rtcerr.OperationError{Err: ErrMaxDataChannelID}
}

// SetMediaEngine allows overwriting the default media engine used by the RTCPeerConnection
// This enables RTCPeerConnection with support for different codecs
func (pc *RTCPeerConnection) SetMediaEngine(m *MediaEngine) {
	pc.mediaEngine = m
}

// SetIdentityProvider is used to configure an identity provider to generate identity assertions
func (pc *RTCPeerConnection) SetIdentityProvider(provider string) error {
	return errors.Errorf("TODO SetIdentityProvider")
}

// SendRTCP sends a user provided RTCP packet to the connected peer
// If no peer is connected the packet is discarded
func (pc *RTCPeerConnection) SendRTCP(pkt rtcp.Packet) error {
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	pc.networkManager.SendRTCP(raw)
	return nil
}

// Close ends the RTCPeerConnection
func (pc *RTCPeerConnection) Close() error {
	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #2)
	if pc.isClosed {
		return nil
	}

	close(pc.backgroundActions)

	pc.networkManager.Close()

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #3)
	pc.isClosed = true

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #4)
	pc.SignalingState = RTCSignalingStateClosed

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #11)
	// pc.IceConnectionState = RTCIceConnectionStateClosed
	pc.IceConnectionState = ice.ConnectionStateClosed // FIXME REMOVE

	// https://www.w3.org/TR/webrtc/#dom-rtcpeerconnection-close (step #12)
	pc.ConnectionState = RTCPeerConnectionStateClosed

	return nil
}

/* Everything below is private */
func (pc *RTCPeerConnection) generateChannel(ssrc uint32, payloadType uint8) (buffers chan<- *rtp.Packet) {
	if pc.OnTrack == nil {
		return nil
	}

	sdpCodec, err := pc.CurrentLocalDescription.parsed.GetCodecForPayloadType(payloadType)
	if err != nil {
		fmt.Printf("No codec could be found in RemoteDescription for payloadType %d \n", payloadType)
		return nil
	}

	codec, err := pc.mediaEngine.getCodecSDP(sdpCodec)
	if err != nil {
		fmt.Printf("Codec %s in not registered\n", sdpCodec)
		return nil
	}

	bufferTransport := make(chan *rtp.Packet, 15)

	track := &RTCTrack{
		PayloadType: payloadType,
		Kind:        codec.Type,
		ID:          "0", // TODO extract from remoteDescription
		Label:       "",  // TODO extract from remoteDescription
		Ssrc:        ssrc,
		Codec:       codec,
		Packets:     bufferTransport,
	}

	// TODO: Register the receiving Track

	go pc.OnTrack(track)
	return bufferTransport
}

func (pc *RTCPeerConnection) iceStateChange(newState ice.ConnectionState) {
	pc.Lock()
	defer pc.Unlock()

	if pc.OnICEConnectionStateChange != nil {
		pc.OnICEConnectionStateChange(newState)
	}
	pc.IceConnectionState = newState
}

func (pc *RTCPeerConnection) dataChannelEventHandler(e network.DataChannelEvent) {
	pc.Lock()
	defer pc.Unlock()

	switch event := e.(type) {
	case *network.DataChannelCreated:
		id := event.StreamIdentifier()
		newDataChannel := &RTCDataChannel{ID: &id, Label: event.Label, rtcPeerConnection: pc, ReadyState: RTCDataChannelStateOpen}
		pc.dataChannels[e.StreamIdentifier()] = newDataChannel
		if pc.OnDataChannel != nil {
			pc.backgroundActions <- func() {
				pc.OnDataChannel(newDataChannel) // This should actually be called when processing the SDP answer.
				if newDataChannel.OnOpen != nil {
					newDataChannel.doOnOpen()
				}
			}
		} else {
			fmt.Println("OnDataChannel is unset, discarding message")
		}
	case *network.DataChannelMessage:
		if datachannel, ok := pc.dataChannels[e.StreamIdentifier()]; ok {
			datachannel.RLock()
			defer datachannel.RUnlock()

			if datachannel.Onmessage != nil {
				pc.backgroundActions <- func() { datachannel.Onmessage(event.Payload) }
			} else {
				fmt.Printf("Onmessage has not been set for Datachannel %s %d \n", datachannel.Label, e.StreamIdentifier())
			}
		} else {
			fmt.Printf("No datachannel found for streamIdentifier %d \n", e.StreamIdentifier())

		}
	case *network.DataChannelOpen:
		for _, dc := range pc.dataChannels {
			dc.Lock()
			err := dc.sendOpenChannelMessage()
			if err != nil {
				fmt.Println("failed to send openchannel", err)
				dc.Unlock()
				continue
			}
			dc.ReadyState = RTCDataChannelStateOpen
			dc.Unlock()

			pc.backgroundActions <- func() {
				dc.doOnOpen() // TODO: move to ChannelAck handling
			}
		}
	default:
		fmt.Printf("Unhandled DataChannelEvent %v \n", event)
	}
}

func (pc *RTCPeerConnection) generateLocalCandidates() []string {
	pc.networkManager.IceAgent.RLock()
	defer pc.networkManager.IceAgent.RUnlock()

	candidates := make([]string, 0)
	for _, c := range pc.networkManager.IceAgent.LocalCandidates {
		candidates = append(candidates, sdp.ICECandidateMarshal(c)...)
	}
	return candidates
}

func localDirection(weSend bool, peerDirection RTCRtpTransceiverDirection) RTCRtpTransceiverDirection {
	theySend := (peerDirection == RTCRtpTransceiverDirectionSendrecv || peerDirection == RTCRtpTransceiverDirectionSendonly)
	if weSend && theySend {
		return RTCRtpTransceiverDirectionSendrecv
	} else if weSend && !theySend {
		return RTCRtpTransceiverDirectionSendonly
	} else if !weSend && theySend {
		return RTCRtpTransceiverDirectionRecvonly
	}

	return RTCRtpTransceiverDirectionInactive
}

func (pc *RTCPeerConnection) addRTPMediaSection(d *sdp.SessionDescription, codecType RTCRtpCodecType, midValue string, peerDirection RTCRtpTransceiverDirection, candidates []string, dtlsRole sdp.ConnectionRole) bool {
	if codecs := pc.mediaEngine.getCodecsByKind(codecType); len(codecs) == 0 {
		return false
	}

	media := sdp.NewJSEPMediaDescription(codecType.String(), []string{}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()). // TODO: Support other connection types
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithICECredentials(pc.networkManager.IceAgent.LocalUfrag, pc.networkManager.IceAgent.LocalPwd).
		WithPropertyAttribute(sdp.AttrKeyRtcpMux).  // TODO: support RTCP fallback
		WithPropertyAttribute(sdp.AttrKeyRtcpRsize) // TODO: Support Reduced-Size RTCP?

	for _, codec := range pc.mediaEngine.getCodecsByKind(codecType) {
		media.WithCodec(codec.PayloadType, codec.Name, codec.ClockRate, codec.Channels, codec.SdpFmtpLine)
	}

	weSend := false
	for _, transceiver := range pc.rtpTransceivers {
		if transceiver.Sender == nil ||
			transceiver.Sender.Track == nil ||
			transceiver.Sender.Track.Kind != codecType {
			continue
		}
		weSend = true
		track := transceiver.Sender.Track
		media = media.WithMediaSource(track.Ssrc, track.Label /* cname */, track.Label /* streamLabel */, track.Label)
	}
	media = media.WithPropertyAttribute(localDirection(weSend, peerDirection).String())

	for _, c := range candidates {
		media.WithCandidate(c)
	}
	media.WithPropertyAttribute("end-of-candidates")
	d.WithMedia(media)
	return true
}

func (pc *RTCPeerConnection) addDataMediaSection(d *sdp.SessionDescription, midValue string, candidates []string, dtlsRole sdp.ConnectionRole) {
	media := (&sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "application",
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"DTLS", "SCTP"},
			Formats: []int{5000},
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address: &sdp.Address{
				IP: net.ParseIP("0.0.0.0"),
			},
		},
	}).
		WithValueAttribute(sdp.AttrKeyConnectionSetup, dtlsRole.String()). // TODO: Support other connection types
		WithValueAttribute(sdp.AttrKeyMID, midValue).
		WithPropertyAttribute(RTCRtpTransceiverDirectionSendrecv.String()).
		WithPropertyAttribute("sctpmap:5000 webrtc-datachannel 1024").
		WithICECredentials(pc.networkManager.IceAgent.LocalUfrag, pc.networkManager.IceAgent.LocalPwd)

	for _, c := range candidates {
		media.WithCandidate(c)
	}
	media.WithPropertyAttribute("end-of-candidates")

	d.WithMedia(media)
}

func (pc *RTCPeerConnection) newRTCTrack(payloadType uint8, ssrc uint32, id, label string) (*RTCTrack, error) {
	codec, err := pc.mediaEngine.getCodec(payloadType)
	if err != nil {
		return nil, err
	}

	if codec.Payloader == nil {
		return nil, errors.New("codec payloader not set")
	}

	trackInput := make(chan media.RTCSample, 15) // Is the buffering needed?
	rawPackets := make(chan *rtp.Packet)
	if ssrc == 0 {
		buf := make([]byte, 4)
		_, err = rand.Read(buf)
		if err != nil {
			return nil, errors.New("failed to generate random value")
		}
		ssrc = binary.LittleEndian.Uint32(buf)

		go func() {
			packetizer := rtp.NewPacketizer(
				1400,
				payloadType,
				ssrc,
				codec.Payloader,
				rtp.NewRandomSequencer(),
				codec.ClockRate,
			)

			for {
				in := <-trackInput
				packets := packetizer.Packetize(in.Data, in.Samples)
				for _, p := range packets {
					pc.networkManager.SendRTP(p)
				}
			}
		}()
		close(rawPackets)
	} else {
		// If SSRC is not 0, then we are working with an established RTP stream
		// and need to accept raw RTP packets for forwarding.
		go func() {
			for {
				p := <-rawPackets
				pc.networkManager.SendRTP(p)
			}
		}()
		close(trackInput)
	}

	t := &RTCTrack{
		PayloadType: payloadType,
		Kind:        codec.Type,
		ID:          id,
		Label:       label,
		Ssrc:        ssrc,
		Codec:       codec,
		Samples:     trackInput,
		RawRTP:      rawPackets,
	}

	return t, nil
}

// NewRawRTPTrack initializes a new *RTCTrack configured to accept raw *rtp.Packet
//
// NB: If the source RTP stream is being broadcast to multiple tracks, each track
// must receive its own copies of the source packets in order to avoid packet corruption.
func (pc *RTCPeerConnection) NewRawRTPTrack(payloadType uint8, ssrc uint32, id, label string) (*RTCTrack, error) {
	if ssrc == 0 {
		return nil, errors.New("SSRC supplied to NewRawRTPTrack() must be non-zero")
	}
	return pc.newRTCTrack(payloadType, ssrc, id, label)
}

// NewRTCSampleTrack initializes a new *RTCTrack configured to accept media.RTCSample
func (pc *RTCPeerConnection) NewRTCSampleTrack(payloadType uint8, id, label string) (*RTCTrack, error) {
	return pc.newRTCTrack(payloadType, 0, id, label)
}

// NewRTCTrack is used to create a new RTCTrack
//
// Deprecated: Use NewRTCSampleTrack() instead
func (pc *RTCPeerConnection) NewRTCTrack(payloadType uint8, id, label string) (*RTCTrack, error) {
	return pc.NewRTCSampleTrack(payloadType, id, label)
}

func (pc *RTCPeerConnection) newRTCRtpTransceiver(
	receiver *RTCRtpReceiver,
	sender *RTCRtpSender,
	direction RTCRtpTransceiverDirection,
) *RTCRtpTransceiver {

	t := &RTCRtpTransceiver{
		Receiver:  receiver,
		Sender:    sender,
		Direction: direction,
	}
	pc.rtpTransceivers = append(pc.rtpTransceivers, t)
	return t
}
