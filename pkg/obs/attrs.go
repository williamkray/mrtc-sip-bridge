package obs

import "go.opentelemetry.io/otel/attribute"

// Attribute keys of TELEMETRY-SPEC §5.2 (plus the closed-enum metric attributes of §4.3).
const (
	AttrCallID         = attribute.Key("call.id")
	AttrCallDirection  = attribute.Key("call.direction")
	AttrCallOutcome    = attribute.Key("call.outcome")
	AttrCallTrigger    = attribute.Key("call.trigger")
	AttrCallHangupBy   = attribute.Key("call.hangup.by")
	AttrCallEndReason  = attribute.Key("call.end_reason")
	AttrCallDurationS  = attribute.Key("call.duration_s")
	AttrCallRemoteE164 = attribute.Key("call.remote.e164")
	AttrCallRemoteWith = attribute.Key("call.remote.withheld")
	AttrCallLocalE164  = attribute.Key("call.local.e164")

	AttrSIPMethod            = attribute.Key("sip.method")
	AttrSIPCallID            = attribute.Key("sip.call_id")
	AttrSIPCSeq              = attribute.Key("sip.cseq")
	AttrSIPStatusCode        = attribute.Key("sip.response.status_code")
	AttrSIPStatusClass       = attribute.Key("sip.response.status_class")
	AttrSIPReason            = attribute.Key("sip.response.reason")
	AttrSIPRegisterKind      = attribute.Key("sip.register.kind")
	AttrSIPRegisterAttempt   = attribute.Key("sip.register.attempt")
	AttrSIPRegisterNextRetry = attribute.Key("sip.register.next_retry_ms")
	AttrSIPRegistrarAddress  = attribute.Key("sip.registrar.address")
	AttrSIPContact           = attribute.Key("sip.contact")
	AttrSIPUserMasked        = attribute.Key("sip.user.masked")
	AttrSIPAuthChallenged    = attribute.Key("sip.auth.challenged")
	AttrNetworkTransport     = attribute.Key("network.transport")
	AttrNetworkPeerAddress   = attribute.Key("network.peer.address")
	AttrNetworkPeerPort      = attribute.Key("network.peer.port")
	AttrServerAddress        = attribute.Key("server.address")
	AttrServerPort           = attribute.Key("server.port")
	AttrHTTPRequestMethod    = attribute.Key("http.request.method")
	AttrHTTPResponseStatus   = attribute.Key("http.response.status_code")
	AttrURLTemplate          = attribute.Key("url.template")
	AttrErrorType            = attribute.Key("error.type")
	AttrMatrixRoomID         = attribute.Key("matrix.room_id")
	AttrMatrixEventID        = attribute.Key("matrix.event_id")
	AttrMatrixEventType      = attribute.Key("matrix.event.type")
	AttrMatrixSender         = attribute.Key("matrix.sender")
	AttrMatrixGhostID        = attribute.Key("matrix.ghost_id")
	AttrMatrixOwnerID        = attribute.Key("matrix.owner_id")
	AttrMatrixRTCKind        = attribute.Key("matrixrtc.kind")
	AttrMatrixRTCDeviceID    = attribute.Key("matrixrtc.membership.device_id")
	AttrMatrixRTCExpiresMS   = attribute.Key("matrixrtc.membership.expires_ms")
	AttrLiveKitRoom          = attribute.Key("livekit.room")
	AttrLiveKitIdentity      = attribute.Key("livekit.participant.identity")
	AttrLiveKitServer        = attribute.Key("livekit.server.address")
	AttrMediaCodec           = attribute.Key("media.codec")
	AttrMediaPtimeMS         = attribute.Key("media.ptime_ms")
	AttrMediaTelephoneEvent  = attribute.Key("media.telephone_event")
	AttrRTPPacketsRx         = attribute.Key("rtp.packets.rx")
	AttrRTPPacketsTx         = attribute.Key("rtp.packets.tx")
	AttrRTPLost              = attribute.Key("rtp.lost")
	AttrRTPLate              = attribute.Key("rtp.late")
	AttrRTPJitterMaxMS       = attribute.Key("rtp.jitter.max_ms")
	AttrRTPJitterMeanMS      = attribute.Key("rtp.jitter.mean_ms")
	AttrCmdName              = attribute.Key("cmd.name")
	AttrBridgeVersion        = attribute.Key("bridge.version")

	// Phase 2 (PHASE2-SPEC 4.4).
	AttrMediaDirection    = attribute.Key("media.direction")     // metric: rx, tx
	AttrMediaLeg          = attribute.Key("media.leg")           // metric: sip_to_rtc, rtc_to_sip
	AttrMatrixRTCRoutedTo = attribute.Key("matrixrtc.routed_to") // call, none
	AttrRingLifetimeMS    = attribute.Key("matrixrtc.ring.lifetime_ms")
	AttrRTCJoinAttempt    = attribute.Key("rtc.join.attempt")
	AttrLiveKitDisconnect = attribute.Key("livekit.disconnect_reason")

	// Phase 3 (PHASE3-SPEC 3.3).
	AttrSIPEarlyMedia  = attribute.Key("sip.early_media")  // call.ringback: early media was relayed
	AttrDTMFDigitCount = attribute.Key("dtmf.digit_count") // cmd.dtmf: number of digits (never the digits)
	AttrDTMFMethod     = attribute.Key("dtmf.method")      // metric: rfc4733

	// MatrixRTC 2.0 (docs/MATRIXRTC2-SPEC.md WP6). Closed enums; slot ids are a fixed small set.
	AttrMatrixRTCFormat       = attribute.Key("matrixrtc.membership.format") // legacy, sticky
	AttrMatrixRTCSlotID       = attribute.Key("matrixrtc.slot_id")
	AttrMatrixRTCSlotAction   = attribute.Key("matrixrtc.slot.action")       // noop, opened, refused_encrypted
	AttrMatrixRTCMemberType   = attribute.Key("matrixrtc.member.event_type") // event type name sent
	AttrMatrixRTCDelayedLeave = attribute.Key("matrixrtc.delayed_leave")     // bool: a delayed leave was scheduled
	AttrMatrixRTCLeaveVia     = attribute.Key("matrixrtc.member.leave_via")  // delayed_send, explicit
	AttrRTCTokenEndpoint      = attribute.Key("rtc.token.endpoint")          // sfu_get, get_token_cs

	// MatrixRTC 2.0 planning and fallbacks (pkg/calls format.go, rtcmember.go): which dialect was chosen,
	// why, and which fallback was taken. Closed enums except the booleans.
	AttrMatrixRTCPlanFormat       = attribute.Key("matrixrtc.plan.format")       // legacy, sticky, dual
	AttrMatrixRTCPlanReason       = attribute.Key("matrixrtc.plan.reason")       // see calls/format.go reason* constants
	AttrMatrixRTCPlanDowngrade    = attribute.Key("matrixrtc.plan.downgrade")    // slot_error, sticky_unsupported, sticky_error
	AttrMatrixRTCStickySupported  = attribute.Key("matrixrtc.sticky.supported")  // bool
	AttrMatrixRTCDelayedSupported = attribute.Key("matrixrtc.delayed.supported") // bool
	AttrMatrixRTCTransports       = attribute.Key("matrixrtc.transports.status") // not_needed, ok, no_livekit_url, error
	AttrRTCTokenFallback          = attribute.Key("rtc.token.fallback")          // bool: MSC4195 get_token failed, sfu_get used

	// Closed-enum attributes used only on metrics.
	AttrOutcome = attribute.Key("outcome")
	AttrState   = attribute.Key("state")
	AttrRouted  = attribute.Key("routed")
)
