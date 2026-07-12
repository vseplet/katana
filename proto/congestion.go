package main

// Настоящий WebRTC-конгешн-контроль (Google Congestion Control) вместо нашего
// самопального loss-based AIMD + отсутствия пейсинга.
//
// Почему: наш AIMD реагировал на потери ПОСЛЕ факта (кадр уже развалился) и
// осциллировал на шумных RR, а кадры уходили микро-залпами (пейсера нет) — часть
// потерь мы создавали сами. GCC делает как Chrome/Meet: delay-based оценка полосы
// (ловит затор по росту задержки ДО потерь) + leaky-bucket пейсер (ровная отправка,
// без залпов) + TWCC (точные тайминги прихода каждого пакета от зрителя).
//
// ТОЛЬКО Windows: там нативный энкодер и проблема с периодическими фризами. mac/linux
// остаются на прежнем пути (не трогаем паритет). Откат на лету: KATANA_NO_GCC=1.

import (
	"os"
	"runtime"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"
)

func gccEnabled() bool {
	return runtime.GOOS == "windows" && os.Getenv("KATANA_NO_GCC") != "1"
}

// gccEstimators — канал, куда cc-интерсептор кладёт BWE-оценщик каждого нового
// PeerConnection. buildLocked (под h.mu, последовательно) забирает оценщик своего PC.
var gccEstimators = make(chan cc.BandwidthEstimator, 8)

// newGCCAPI строит webrtc.API с полным стеком GCC: кодеки с transport-cc фидбеком,
// TWCC header-extension, cc-интерсептор (delay-based BWE + leaky-bucket пейсер).
func newGCCAPI() *webrtc.API {
	m := &webrtc.MediaEngine{}
	if err := registerCodecsTWCC(m); err != nil {
		panic(err)
	}
	ir := &interceptor.Registry{}

	ccFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(2_000_000),
			gcc.SendSideBWEMinBitrate(1_000_000), // пол — как наш AIMD-floor
			gcc.SendSideBWEMaxBitrate(6_000_000),
		)
	})
	if err != nil {
		panic(err)
	}
	ccFactory.OnNewPeerConnection(func(_ string, est cc.BandwidthEstimator) {
		select {
		case gccEstimators <- est:
		default:
		}
	})
	ir.Add(ccFactory)

	// TWCC header-extension (сендер добавляет transport-seq к исходящим пакетам,
	// зритель шлёт по ним transport-cc фидбек, который читает GCC).
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		panic(err)
	}
	// NACK-ретрансмит + RTCP отчёты (как в дефолте).
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		panic(err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
}

// registerCodecsTWCC регистрирует те же кодеки, что RegisterDefaultCodecs, но с
// добавленным transport-cc в видео-фидбек (без него зритель не шлёт TWCC → GCC слеп).
// Профили H264 повторяют дефолтные, чтобы браузер гарантированно состыковался.
func registerCodecsTWCC(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}

	fb := []webrtc.RTCPFeedback{
		{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"},
		{Type: "nack"}, {Type: "nack", Parameter: "pli"},
		{Type: "transport-cc"},
	}
	h264 := func(fmtp string) webrtc.RTPCodecCapability {
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp, RTCPFeedback: fb}
	}
	rtx := func(apt string) webrtc.RTPCodecCapability {
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: apt}
	}
	video := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: 96},
		{RTPCodecCapability: rtx("apt=96"), PayloadType: 97},
		{RTPCodecCapability: h264("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"), PayloadType: 102},
		{RTPCodecCapability: rtx("apt=102"), PayloadType: 103},
		{RTPCodecCapability: h264("level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"), PayloadType: 104},
		{RTPCodecCapability: rtx("apt=104"), PayloadType: 105},
		{RTPCodecCapability: h264("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"), PayloadType: 106},
		{RTPCodecCapability: rtx("apt=106"), PayloadType: 107},
		{RTPCodecCapability: h264("level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"), PayloadType: 108},
		{RTPCodecCapability: rtx("apt=108"), PayloadType: 109},
		{RTPCodecCapability: h264("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"), PayloadType: 127},
		{RTPCodecCapability: rtx("apt=127"), PayloadType: 125},
	}
	for _, c := range video {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}
