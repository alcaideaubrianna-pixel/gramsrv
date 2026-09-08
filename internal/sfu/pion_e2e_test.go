package sfu

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"
	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"go.uber.org/zap/zaptest"
)

var errClientDTLSStateMissing = errors.New("client dtls state missing")

// fakeTgcallsClient 按 tgcalls GroupNetworkManager 的角色契约模拟客户端：
// ICE CONTROLLED（等待 SFU 发起连通性检查）、DTLS setup="passive"（server 角色，
// 等 SFU 主动握手）、SRTP isClient=false。
type fakeTgcallsClient struct {
	t           *testing.T
	ufrag, pwd  string
	cert        tls.Certificate
	fingerprint string
	ssrc        uint32
	groups      []SsrcGroup

	session *srtp.SessionSRTP
	write   *srtp.WriteStreamSRTP
	closeFn []func()
}

func newFakeTgcallsClient(t *testing.T, ssrc uint32) *fakeTgcallsClient {
	t.Helper()
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatalf("client cert: %v", err)
	}
	fp, err := certificateFingerprint(cert.Certificate[0])
	if err != nil {
		t.Fatalf("client fingerprint: %v", err)
	}
	ufrag, _ := randomICEString(8)
	pwd, _ := randomICEString(24)
	return &fakeTgcallsClient{t: t, ufrag: ufrag, pwd: pwd, cert: cert, fingerprint: fp, ssrc: ssrc}
}

func (c *fakeTgcallsClient) offer() ClientOffer {
	return ClientOffer{
		AudioSSRC:         c.ssrc,
		Ufrag:             c.ufrag,
		Pwd:               c.pwd,
		FingerprintSHA256: c.fingerprint,
		SsrcGroups:        c.groups,
	}
}

// connect 完成 ICE(controlled)+DTLS(server)+SRTP 建链。
func (c *fakeTgcallsClient) connect(ctx context.Context, answer ServerAnswer) error {
	return c.connectWithICERole(ctx, answer, false)
}

func (c *fakeTgcallsClient) connectWithICERole(ctx context.Context, answer ServerAnswer, controlling bool) error {
	agent, err := ice.NewAgent(&ice.AgentConfig{
		NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4},
		CandidateTypes:  []ice.CandidateType{ice.CandidateTypeHost},
		LocalUfrag:      c.ufrag,
		LocalPwd:        c.pwd,
		IncludeLoopback: true,
	})
	if err != nil {
		return err
	}
	c.closeFn = append(c.closeFn, func() { _ = agent.Close() })
	if err := agent.OnCandidate(func(ice.Candidate) {}); err != nil {
		return err
	}
	if err := agent.GatherCandidates(); err != nil {
		return err
	}
	for _, cand := range answer.Candidates {
		remote, err := ice.NewCandidateHost(&ice.CandidateHostConfig{
			Network:   "udp",
			Address:   cand.IP,
			Port:      cand.Port,
			Component: 1,
		})
		if err != nil {
			return err
		}
		if err := agent.AddRemoteCandidate(remote); err != nil {
			return err
		}
	}
	var conn *ice.Conn
	if controlling {
		conn, err = agent.Dial(ctx, answer.Ufrag, answer.Pwd)
	} else {
		conn, err = agent.Accept(ctx, answer.Ufrag, answer.Pwd)
	}
	if err != nil {
		return err
	}
	demux := newDemuxer(conn)
	c.closeFn = append(c.closeFn, demux.Close)
	dtlsRaw := demux.dtlsConn()
	dtlsConn, err := dtls.Server(dtlsnet.PacketConnFromConn(dtlsRaw), dtlsRaw.RemoteAddr(), &dtls.Config{
		Certificates:         []tls.Certificate{c.cert},
		ClientAuth:           dtls.RequireAnyClientCert,
		InsecureSkipVerify:   true,
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
		},
	})
	if err != nil {
		return err
	}
	hsCtx, hsCancel := context.WithTimeout(ctx, 20*time.Second)
	err = dtlsConn.HandshakeContext(hsCtx)
	hsCancel()
	if err != nil {
		return err
	}
	state, ok := dtlsConn.ConnectionState()
	if !ok {
		return errClientDTLSStateMissing
	}
	// 服务端证书指纹应等于信令面下发的 answer.FingerprintSHA256。
	gotFP, err := certificateFingerprint(state.PeerCertificates[0])
	if err != nil || normalizeFingerprint(gotFP) != normalizeFingerprint(answer.FingerprintSHA256) {
		return fmt.Errorf("sfu fingerprint mismatch: %v / %s vs %s", err, gotFP, answer.FingerprintSHA256)
	}
	profile, _ := dtlsConn.SelectedSRTPProtectionProfile()
	srtpConfig := &srtp.Config{}
	switch profile {
	case dtls.SRTP_AEAD_AES_128_GCM:
		srtpConfig.Profile = srtp.ProtectionProfileAeadAes128Gcm
	case dtls.SRTP_AES128_CM_HMAC_SHA1_80:
		srtpConfig.Profile = srtp.ProtectionProfileAes128CmHmacSha1_80
	default:
		return fmt.Errorf("unexpected srtp profile %v", profile)
	}
	if err := srtpConfig.ExtractSessionKeysFromDTLS(&state, false); err != nil {
		return err
	}
	c.session, err = srtp.NewSessionSRTP(demux.srtpConn(), srtpConfig)
	if err != nil {
		return err
	}
	c.write, err = c.session.OpenWriteStream()
	return err
}

func (c *fakeTgcallsClient) close() {
	for i := len(c.closeFn) - 1; i >= 0; i-- {
		c.closeFn[i]()
	}
}

// sendOpusPacket 构造带 audio-level 扩展（one-byte header，id=1）的 RTP 包。
func (c *fakeTgcallsClient) sendOpusPacket(seq uint16, payload []byte, audioLevel byte) error {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111, // opus
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 960,
			SSRC:           c.ssrc,
		},
		Payload: payload,
	}
	pkt.Header.Extension = true
	pkt.Header.ExtensionProfile = 0xBEDE
	if err := pkt.Header.SetExtension(1, []byte{audioLevel}); err != nil {
		return err
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = c.write.Write(raw)
	return err
}

// sendVideoPacket 构造指定 ssrc 的视频 RTP 包（VP8 PT=100）。
func (c *fakeTgcallsClient) sendVideoPacket(ssrc uint32, seq uint16) error {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    100, // VP8
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 3000,
			SSRC:           ssrc,
		},
		Payload: []byte{0x90, 0x00, byte(seq)},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = c.write.Write(raw)
	return err
}

// 视频选层：发布端三层 simulcast，SFU 只转发 SIM[0] 层（订阅端只为该 ssrc 建
// 解码 sink，高层包客户端会丢弃，SFU 侧直接终结省下行）。
func TestPionSFUAcceptsWebAControllingICEClient(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e sfu test")
	}
	port := pickUDPPort(t)
	svc, err := NewPion(PionConfig{UDPPort: port, AdvertiseIP: "127.0.0.1", Logger: zaptest.NewLogger(t)})
	if err != nil {
		t.Fatalf("new pion sfu: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newFakeTgcallsClient(t, 0x700)
	defer client.close()
	answer, err := svc.Join(ctx, 902, 1, EndpointMain, client.offer())
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := client.connectWithICERole(ctx, answer, true); err != nil {
		t.Fatalf("controlling client connect: %v", err)
	}
}

func TestPionSFUForwardsOnlyBaseSimulcastLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e sfu test")
	}
	port := pickUDPPort(t)
	svc, err := NewPion(PionConfig{UDPPort: port, AdvertiseIP: "127.0.0.1", Logger: zaptest.NewLogger(t)})
	if err != nil {
		t.Fatalf("new pion sfu: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const callID = int64(901)
	alice := newFakeTgcallsClient(t, 0x500)
	base := alice.ssrc + 1
	alice.groups = []SsrcGroup{
		{Semantics: "SIM", Sources: []uint32{base, base + 2, base + 4}},
		{Semantics: "FID", Sources: []uint32{base, base + 1}},
		{Semantics: "FID", Sources: []uint32{base + 2, base + 3}},
		{Semantics: "FID", Sources: []uint32{base + 4, base + 5}},
	}
	bob := newFakeTgcallsClient(t, 0x600)
	defer alice.close()
	defer bob.close()

	answerA, err := svc.Join(ctx, callID, 1, EndpointMain, alice.offer())
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	answerB, err := svc.Join(ctx, callID, 2, EndpointMain, bob.offer())
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}
	errCh := make(chan error, 2)
	go func() { errCh <- alice.connect(ctx, answerA) }()
	go func() { errCh <- bob.connect(ctx, answerB) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("client connect: %v", err)
		}
	}

	// alice 同时在 layer0（应转发）与 layer1（应被 SFU 终结）发包。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		seq := uint16(1)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = alice.sendVideoPacket(base, seq)
				_ = alice.sendVideoPacket(base+2, seq)
				seq++
			}
		}
	}()

	// bob 第一条转发流必须是 layer0；观察窗口内绝不能出现 layer1。
	type accepted struct{ ssrc uint32 }
	got := make(chan accepted, 4)
	go func() {
		for {
			stream, ssrc, err := bob.session.AcceptStream()
			if err != nil {
				return
			}
			got <- accepted{ssrc}
			go func() {
				buf := make([]byte, 1500)
				for {
					if _, err := stream.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	deadline := time.After(10 * time.Second)
	sawBase := false
	for !sawBase {
		select {
		case in := <-got:
			if in.ssrc == base+2 {
				t.Fatalf("higher simulcast layer %#x leaked through SFU", in.ssrc)
			}
			if in.ssrc == base {
				sawBase = true
			}
		case <-deadline:
			t.Fatalf("bob never received base layer stream")
		}
	}
	// 再观察一段时间确认高层不漏。
	quiet := time.After(2 * time.Second)
	for {
		select {
		case in := <-got:
			if in.ssrc == base+2 {
				t.Fatalf("higher simulcast layer %#x leaked through SFU", in.ssrc)
			}
		case <-quiet:
			_ = svc.CloseRoom(ctx, callID)
			return
		}
	}
}

func TestPionSFUForwardsOpusBetweenClients(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e sfu test")
	}
	port := pickUDPPort(t)
	svc, err := NewPion(PionConfig{
		UDPPort:     port,
		AdvertiseIP: "127.0.0.1",
		Logger:      zaptest.NewLogger(t),
	})
	if err != nil {
		t.Fatalf("new pion sfu: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const callID = int64(900)
	alice := newFakeTgcallsClient(t, 0xA11CE)
	bob := newFakeTgcallsClient(t, 0xB0B)
	defer alice.close()
	defer bob.close()

	answerA, err := svc.Join(ctx, callID, 1, EndpointMain, alice.offer())
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	if len(answerA.Candidates) != 1 || answerA.Candidates[0].Port != port {
		t.Fatalf("answer candidates = %+v", answerA.Candidates)
	}
	answerB, err := svc.Join(ctx, callID, 2, EndpointMain, bob.offer())
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}
	// 两端建链（ICE prflx → DTLS 由 SFU 主动握手 → SRTP）。
	errCh := make(chan error, 2)
	go func() { errCh <- alice.connect(ctx, answerA) }()
	go func() { errCh <- bob.connect(ctx, answerB) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("client connect: %v", err)
		}
	}

	// alice 持续发包（SFU AcceptStream 在首包后建立读流）。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		seq := uint16(1)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = alice.sendOpusPacket(seq, []byte{0xDE, 0xAD, byte(seq)}, 0x7F)
				seq++
			}
		}
	}()

	// bob 收到来自 alice 的转发流：SSRC 不重写、扩展保留。
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 15*time.Second)
	defer acceptCancel()
	type accepted struct {
		stream *srtp.ReadStreamSRTP
		ssrc   uint32
	}
	got := make(chan accepted, 1)
	go func() {
		stream, ssrc, err := bob.session.AcceptStream()
		if err == nil {
			got <- accepted{stream, ssrc}
		}
	}()
	var inbound accepted
	select {
	case inbound = <-got:
	case <-acceptCtx.Done():
		t.Fatalf("bob never received forwarded stream")
	}
	if inbound.ssrc != alice.ssrc {
		t.Fatalf("forwarded ssrc = %#x, want alice %#x（SFU 不得重写 SSRC）", inbound.ssrc, alice.ssrc)
	}
	buf := make([]byte, 1500)
	n, err := inbound.stream.Read(buf)
	if err != nil {
		t.Fatalf("bob read: %v", err)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal forwarded packet: %v", err)
	}
	if pkt.PayloadType != 111 || pkt.SSRC != alice.ssrc {
		t.Fatalf("forwarded packet = %+v", pkt.Header)
	}
	if ext := pkt.Header.GetExtension(1); len(ext) != 1 || ext[0] != 0x7F {
		t.Fatalf("audio-level extension lost: %v（speaking 指示依赖逐包透传）", ext)
	}

	// 媒体面活性：双端都应在 alive 集合（alice 发包、bob 收包均计活性…bob 仅收不发，
	// 其活性来自 SFU 写出？写出不计；bob 至少 connected+握手活性在窗口内）。
	alive := svc.AliveUserIDs(callID)
	aliveSet := map[int64]bool{}
	for _, id := range alive {
		aliveSet[id] = true
	}
	if !aliveSet[1] {
		t.Fatalf("alice must be media-alive, got %v", alive)
	}

	if err := svc.Leave(ctx, callID, 1, EndpointMain); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := svc.CloseRoom(ctx, callID); err != nil {
		t.Fatalf("close room: %v", err)
	}
}

func pickUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}
