package ssh

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gerror "github.com/fatedier/golib/errors"
	"golang.org/x/crypto/ssh"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/util/log"
)

const (
	// ssh protocol define
	// https://datatracker.ietf.org/doc/html/rfc4254#page-16
	ChannelTypeServerOpenChannel = "forwarded-tcpip"
	RequestTypeForward           = "tcpip-forward"

	// golang ssh package define.
	// https://pkg.go.dev/golang.org/x/crypto/ssh
	RequestTypeHeartbeat = "keepalive@openssh.com"
)

// 当 proxy 失败会返回该错误
type VProxyError struct{}

// ssh protocol define
// https://datatracker.ietf.org/doc/html/rfc4254#page-16
// parse ssh client cmds input
type forwardedTCPPayload struct {
	Addr string
	Port uint32

	// can be default empty value but do not delete it
	// because ssh protocol shoule be reserved
	OriginAddr string
	OriginPort uint32
}

// custom define
// parse ssh client cmds input
type CmdPayload struct {
	Address string
	Port    uint32
}

// custom define
// with frp control cmds
type ExtraPayload struct {
	Type string

	// TODO port can be set by extra message and priority to ssh raw cmd
	Address string
	Port    uint32
}

type Service struct {
	tcpConn net.Conn
	cfg     *ssh.ServerConfig

	sshConn  *ssh.ServerConn
	gChannel <-chan ssh.NewChannel
	gReq     <-chan *ssh.Request

	addrPayloadCh  chan CmdPayload
	extraPayloadCh chan ExtraPayload

	proxyPayloadCh chan v1.ProxyConfigurer
	replyCh        chan interface{}

	closeCh chan struct{}
	exit    int32
}

func NewSSHService(
	tcpConn net.Conn,
	cfg *ssh.ServerConfig,
	proxyPayloadCh chan v1.ProxyConfigurer,
	replyCh chan interface{},
) (ss *Service, err error) {
	ss = &Service{
		tcpConn: tcpConn,
		cfg:     cfg,

		addrPayloadCh:  make(chan CmdPayload),
		extraPayloadCh: make(chan ExtraPayload),

		proxyPayloadCh: proxyPayloadCh,
		replyCh:        replyCh,

		closeCh: make(chan struct{}),
		exit:    0,
	}

	ss.sshConn, ss.gChannel, ss.gReq, err = ssh.NewServerConn(tcpConn, cfg)
	if err != nil {
		log.Error("ssh handshake error: %v", err)
		return nil, err
	}

	log.Info("ssh connection success")

	return ss, nil
}

func (ss *Service) Run() {
	go ss.loopGenerateProxy()
	go ss.loopParseCmdPayload()
	go ss.loopParseExtraPayload()
	go ss.loopReply()
}

func (ss *Service) Exit() <-chan struct{} {
	return ss.closeCh
}

func (ss *Service) Close() {
	if atomic.LoadInt32(&ss.exit) == 1 {
		return
	}

	select {
	case <-ss.closeCh:
		return
	default:
	}

	close(ss.closeCh)
	close(ss.addrPayloadCh)
	close(ss.extraPayloadCh)

	_ = ss.sshConn.Wait()

	ss.sshConn.Close()
	ss.tcpConn.Close()

	atomic.StoreInt32(&ss.exit, 1)

	log.Info("ssh service close")
}

func (ss *Service) loopParseCmdPayload() {
	for {
		select {
		case req, ok := <-ss.gReq:
			if !ok {
				log.Info("global request is close")
				ss.Close()
				return
			}

			switch req.Type {
			case RequestTypeForward:
				var addrPayload CmdPayload
				if err := ssh.Unmarshal(req.Payload, &addrPayload); err != nil {
					log.Error("ssh unmarshal error: %v", err)
					return
				}
				_ = gerror.PanicToError(func() {
					ss.addrPayloadCh <- addrPayload
				})
			default:
				if req.Type == RequestTypeHeartbeat {
					log.Debug("ssh heartbeat data")
				} else {
					log.Info("default req, data: %v", req)
				}
			}
			if req.WantReply {
				err := req.Reply(true, nil)
				if err != nil {
					log.Error("reply to ssh client error: %v", err)
				}
			}
		case <-ss.closeCh:
			log.Info("loop parse cmd payload close")
			return
		}
	}
}

func (ss *Service) loopSendHeartbeat(ch ssh.Channel) {
	tk := time.NewTicker(time.Second * 60)
	defer tk.Stop()

	for {
		select {
		case <-tk.C:
			ok, err := ch.SendRequest("heartbeat", false, nil)
			if err != nil {
				log.Error("channel send req error: %v", err)
				if err == io.EOF {
					ss.Close()
					return
				}
				continue
			}
			log.Debug("heartbeat send success, ok: %v", ok)
		case <-ss.closeCh:
			return
		}
	}
}

func (ss *Service) loopParseExtraPayload() {
	log.Info("loop parse extra payload start")

	for newChannel := range ss.gChannel {
		ch, req, err := newChannel.Accept()
		if err != nil {
			log.Error("channel accept error: %v", err)
			return
		}

		go ss.loopSendHeartbeat(ch)

		go func(req <-chan *ssh.Request) {
			for r := range req {
				if len(r.Payload) <= 4 {
					log.Info("r.payload is less than 4")
					continue
				}
				if !strings.Contains(string(r.Payload), "tcp") && !strings.Contains(string(r.Payload), "http") {
					log.Info("ssh protocol exchange data")
					continue
				}

				// [4byte data_len|data]
				end := 4 + binary.BigEndian.Uint32(r.Payload[:4])
				if end > uint32(len(r.Payload)) {
					end = uint32(len(r.Payload))
				}
				p := string(r.Payload[4:end])

				msg, err := parseSSHExtraMessage(p)
				if err != nil {
					log.Error("parse ssh extra message error: %v, payload: %v", err, r.Payload)
					continue
				}
				_ = gerror.PanicToError(func() {
					ss.extraPayloadCh <- msg
				})
				return
			}
		}(req)
	}
}

func (ss *Service) SSHConn() *ssh.ServerConn {
	return ss.sshConn
}

func (ss *Service) TCPConn() net.Conn {
	return ss.tcpConn
}

func (ss *Service) loopReply() {
	for {
		select {
		case <-ss.closeCh:
			log.Info("loop reply close")
			return
		case req := <-ss.replyCh:
			switch req.(type) {
			case *VProxyError:
				log.Error("run frp proxy error, close ssh service")
				ss.Close()
			default:
				// TODO
			}
		}
	}
}

func (ss *Service) loopGenerateProxy() {
	log.Info("loop generate proxy start")

	for {
		if atomic.LoadInt32(&ss.exit) == 1 {
			return
		}

		wg := new(sync.WaitGroup)
		wg.Add(2)

		var p1 CmdPayload
		var p2 ExtraPayload

		go func() {
			defer wg.Done()
			for {
				select {
				case <-ss.closeCh:
					return
				case p1 = <-ss.addrPayloadCh:
					return
				}
			}
		}()

		go func() {
			defer wg.Done()
			for {
				select {
				case <-ss.closeCh:
					return
				case p2 = <-ss.extraPayloadCh:
					return
				}
			}
		}()

		wg.Wait()

		if atomic.LoadInt32(&ss.exit) == 1 {
			return
		}

		switch p2.Type {
		case "http":
		case "tcp":
			ss.proxyPayloadCh <- &v1.TCPProxyConfig{
				ProxyBaseConfig: v1.ProxyBaseConfig{
					Name: fmt.Sprintf("ssh-proxy-%v-%v", ss.tcpConn.RemoteAddr().String(), time.Now().UnixNano()),
					Type: p2.Type,

					ProxyBackend: v1.ProxyBackend{
						LocalIP: p1.Address,
					},
				},
				RemotePort: int(p1.Port),
			}
		default:
			log.Warn("invalid frp proxy type: %v", p2.Type)
		}
	}
}

func parseSSHExtraMessage(s string) (p ExtraPayload, err error) {
	sn := len(s)

	log.Info("parse ssh extra message: %v", s)

	ss := strings.Fields(s)
	if len(ss) == 0 {
		if sn != 0 {
			ss = append(ss, s)
		} else {
			return p, fmt.Errorf("invalid ssh input, args: %v", ss)
		}
	}

	for i, v := range ss {
		ss[i] = strings.TrimSpace(v)
	}

	if ss[0] != "tcp" && ss[0] != "http" {
		return p, fmt.Errorf("only support tcp/http now")
	}

	switch ss[0] {
	case "tcp":
		tcpCmd, err := ParseTCPCommand(ss)
		if err != nil {
			return ExtraPayload{}, fmt.Errorf("invalid ssh input: %v", err)
		}

		port, _ := strconv.Atoi(tcpCmd.Port)

		p = ExtraPayload{
			Type:    "tcp",
			Address: tcpCmd.Address,
			Port:    uint32(port),
		}
	case "http":
		httpCmd, err := ParseHTTPCommand(ss)
		if err != nil {
			return ExtraPayload{}, fmt.Errorf("invalid ssh input: %v", err)
		}

		_ = httpCmd

		p = ExtraPayload{
			Type: "http",
		}
	}

	return p, nil
}

type HTTPCommand struct {
	Domain        string
	BasicAuthUser string
	BasicAuthPass string
}

func ParseHTTPCommand(params []string) (*HTTPCommand, error) {
	if len(params) < 2 {
		return nil, errors.New("invalid HTTP command")
	}

	var (
		basicAuth     string
		domainURL     string
		basicAuthUser string
		basicAuthPass string
	)

	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	fs.StringVar(&basicAuth, "basic-auth", "", "")
	fs.StringVar(&domainURL, "domain", "", "")

	fs.SetOutput(&nullWriter{}) // Disables usage output

	err := fs.Parse(params[2:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
	}

	if basicAuth != "" {
		authParts := strings.SplitN(basicAuth, ":", 2)
		basicAuthUser = authParts[0]
		if len(authParts) > 1 {
			basicAuthPass = authParts[1]
		}
	}

	httpCmd := &HTTPCommand{
		Domain:        domainURL,
		BasicAuthUser: basicAuthUser,
		BasicAuthPass: basicAuthPass,
	}
	return httpCmd, nil
}

type TCPCommand struct {
	Address string
	Port    string
}

func ParseTCPCommand(params []string) (*TCPCommand, error) {
	if len(params) == 0 || params[0] != "tcp" {
		return nil, errors.New("invalid TCP command")
	}

	if len(params) == 1 {
		return &TCPCommand{}, nil
	}

	var (
		address string
		port    string
	)

	fs := flag.NewFlagSet("tcp", flag.ContinueOnError)
	fs.StringVar(&address, "address", "", "The IP address to listen on")
	fs.StringVar(&port, "port", "", "The port to listen on")
	fs.SetOutput(&nullWriter{}) // Disables usage output

	args := params[1:]
	err := fs.Parse(args)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
	}

	parsedAddr, err := net.ResolveIPAddr("ip", address)
	if err != nil {
		return nil, err
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return nil, err
	}

	tcpCmd := &TCPCommand{
		Address: parsedAddr.String(),
		Port:    port,
	}
	return tcpCmd, nil
}

type nullWriter struct{}

func (w *nullWriter) Write(p []byte) (n int, err error) { return len(p), nil }
