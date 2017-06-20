// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/KunTengRom/xfrps/models/config"
	"github.com/KunTengRom/xfrps/models/msg"
	"github.com/KunTengRom/xfrps/models/plugin"
	"github.com/KunTengRom/xfrps/models/proto/tcp"
	"github.com/KunTengRom/xfrps/models/proto/udp"
	"github.com/KunTengRom/xfrps/utils/errors"
	"github.com/KunTengRom/xfrps/utils/log"
	frpNet "github.com/KunTengRom/xfrps/utils/net"
)

// Proxy defines how to work for different proxy type.
type Proxy interface {
	Run() error

	// InWorkConn accept work connections registered to server.
	InWorkConn(conn frpNet.Conn)
	Close()
	log.Logger
}

func NewProxy(ctl *Control, pxyConf config.ProxyConf) (pxy Proxy) {
	baseProxy := BaseProxy{
		ctl:    ctl,
		Logger: log.NewPrefixLogger(pxyConf.GetName()),
	}
	switch cfg := pxyConf.(type) {
	case *config.TcpProxyConf:
		pxy = &TcpProxy{
			BaseProxy: baseProxy,
			cfg:       cfg,
		}
	case *config.UdpProxyConf:
		pxy = &UdpProxy{
			BaseProxy: baseProxy,
			cfg:       cfg,
		}
	case *config.FtpProxyConf:
		pxy = &FtpProxy{
			BaseProxy:	baseProxy,
			cfg:		cfg,
		}
	case *config.HttpProxyConf:
		pxy = &HttpProxy{
			BaseProxy: baseProxy,
			cfg:       cfg,
		}
	case *config.HttpsProxyConf:
		pxy = &HttpsProxy{
			BaseProxy: baseProxy,
			cfg:       cfg,
		}
	}
	return
}

type BaseProxy struct {
	ctl    *Control
	closed bool
	mu     sync.RWMutex
	log.Logger
}

// TCP
type TcpProxy struct {
	BaseProxy

	cfg         *config.TcpProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *TcpProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *TcpProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *TcpProxy) InWorkConn(conn frpNet.Conn) {
	HandleTcpWorkConnection(&pxy.cfg.LocalSvrConf, pxy.proxyPlugin, &pxy.cfg.BaseProxyConf, conn)
}

// ftp
type FtpProxy struct {
	BaseProxy
	
	cfg			*config.FtpProxyConf
}

func (pxy *FtpProxy) InWorkConn(conn frpNet.Conn) {
	HandleFtpControlConnection(&pxy.cfg.LocalSvrConf, &pxy.cfg.BaseProxyConf, conn)
}

// HTTP
type HttpProxy struct {
	BaseProxy

	cfg         *config.HttpProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *HttpProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *HttpProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *HttpProxy) InWorkConn(conn frpNet.Conn) {
	HandleTcpWorkConnection(&pxy.cfg.LocalSvrConf, pxy.proxyPlugin, &pxy.cfg.BaseProxyConf, conn)
}

// HTTPS
type HttpsProxy struct {
	BaseProxy

	cfg         *config.HttpsProxyConf
	proxyPlugin plugin.Plugin
}

func (pxy *HttpsProxy) Run() (err error) {
	if pxy.cfg.Plugin != "" {
		pxy.proxyPlugin, err = plugin.Create(pxy.cfg.Plugin, pxy.cfg.PluginParams)
		if err != nil {
			return
		}
	}
	return
}

func (pxy *HttpsProxy) Close() {
	if pxy.proxyPlugin != nil {
		pxy.proxyPlugin.Close()
	}
}

func (pxy *HttpsProxy) InWorkConn(conn frpNet.Conn) {
	HandleTcpWorkConnection(&pxy.cfg.LocalSvrConf, pxy.proxyPlugin, &pxy.cfg.BaseProxyConf, conn)
}

// UDP
type UdpProxy struct {
	BaseProxy

	cfg *config.UdpProxyConf

	localAddr *net.UDPAddr
	readCh    chan *msg.UdpPacket

	// include msg.UdpPacket and msg.Ping
	sendCh   chan msg.Message
	workConn frpNet.Conn
}

func (pxy *UdpProxy) Run() (err error) {
	pxy.localAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", pxy.cfg.LocalIp, pxy.cfg.LocalPort))
	if err != nil {
		return
	}
	return
}

func (pxy *UdpProxy) Close() {
	pxy.mu.Lock()
	defer pxy.mu.Unlock()

	if !pxy.closed {
		pxy.closed = true
		if pxy.workConn != nil {
			pxy.workConn.Close()
		}
		if pxy.readCh != nil {
			close(pxy.readCh)
		}
		if pxy.sendCh != nil {
			close(pxy.sendCh)
		}
	}
}

func (pxy *UdpProxy) InWorkConn(conn frpNet.Conn) {
	pxy.Info("incoming a new work connection for udp proxy, %s", conn.RemoteAddr().String())
	// close resources releated with old workConn
	pxy.Close()

	pxy.mu.Lock()
	pxy.workConn = conn
	pxy.readCh = make(chan *msg.UdpPacket, 1024)
	pxy.sendCh = make(chan msg.Message, 1024)
	pxy.closed = false
	pxy.mu.Unlock()

	workConnReaderFn := func(conn net.Conn, readCh chan *msg.UdpPacket) {
		for {
			var udpMsg msg.UdpPacket
			if errRet := msg.ReadMsgInto(conn, &udpMsg); errRet != nil {
				pxy.Warn("read from workConn for udp error: %v", errRet)
				return
			}
			if errRet := errors.PanicToError(func() {
				pxy.Trace("get udp package from workConn: %s", udpMsg.Content)
				readCh <- &udpMsg
			}); errRet != nil {
				pxy.Info("reader goroutine for udp work connection closed: %v", errRet)
				return
			}
		}
	}
	workConnSenderFn := func(conn net.Conn, sendCh chan msg.Message) {
		defer func() {
			pxy.Info("writer goroutine for udp work connection closed")
		}()
		var errRet error
		for rawMsg := range sendCh {
			switch m := rawMsg.(type) {
			case *msg.UdpPacket:
				pxy.Trace("send udp package to workConn: %s", m.Content)
			case *msg.Ping:
				pxy.Trace("send ping message to udp workConn")
			}
			if errRet = msg.WriteMsg(conn, rawMsg); errRet != nil {
				pxy.Error("udp work write error: %v", errRet)
				return
			}
		}
	}
	heartbeatFn := func(conn net.Conn, sendCh chan msg.Message) {
		var errRet error
		for {
			time.Sleep(time.Duration(30) * time.Second)
			if errRet = errors.PanicToError(func() {
				sendCh <- &msg.Ping{}
			}); errRet != nil {
				pxy.Trace("heartbeat goroutine for udp work connection closed")
				break
			}
		}
	}

	go workConnSenderFn(pxy.workConn, pxy.sendCh)
	go workConnReaderFn(pxy.workConn, pxy.readCh)
	go heartbeatFn(pxy.workConn, pxy.sendCh)
	udp.Forwarder(pxy.localAddr, pxy.readCh, pxy.sendCh)
}

func GetFtpPasvInfo(msg string) (port int, err error) {
	if len(msg) < 45 {
		return 0, errors.New("Msg it too short, Impossible")
	}
	
	start := strings.Index(line, "(")
	end := strings.LastIndex(line, ")")
	if start == -1 || end == -1 {
		return 0, errors.New("Invalid PASV response format")
	}
	
	// We have to split the response string
	pasvData := strings.Split(line[start+1:end], ",")

	if len(pasvData) < 6 {
		return 0, errors.New("Invalid PASV response format")
	}

	// Let's compute the port number
	portPart1, err1 := strconv.Atoi(pasvData[4])
	if err1 != nil {
		err = err1
		return
	}

	portPart2, err2 := strconv.Atoi(pasvData[5])
	if err2 != nil {
		err = err2
		return
	}

	// Recompose port
	port = portPart1*256 + portPart2
	return
}

// handler for ftp work connection
func JoinFtpControl(fc io.ReadWriteCloser, fs io.ReadWriteCloser, baseInfo *config.BaseProxyConf) (inCount int32, outCount int32) {
	var {
		n	int32
		err	error
	}
	for {
		data := make([]byte, 1024)
		n, err = fc.Read(data)
		if err != nil {
			continue
		}
		msg := string(data[:n])
		code, _ := strconv.Atoi(msg[:3])
		if code == 227 {
			
		} else {
			fs.Write(data)
		}
		
		n, err = fs.Read(data)
		if err != nil {
			continue
		}
		fc.Write(data)
	}
	
	return
}

func HandleFtpControlConnection(localInfo *config.LocalSvrConf, baseInfo *config.BaseProxyConf, workConn frpNet.Conn) {
	ftpConn, err := frpNet.ConnectTcpServer(fmt.Sprintf("%s:%d", localInfo.LocalIp, localInfo.LocalPort))
	if err != nil {
		workConn.Error("connect to local service [%s:%d] error: %v", localInfo.LocalIp, localInfo.LocalPort, err)
		return
	}
	
	JoinFtpControl(ftpConn, workConn, baseInfo)
}

// Common handler for tcp work connections.
func HandleTcpWorkConnection(localInfo *config.LocalSvrConf, proxyPlugin plugin.Plugin,
							baseInfo *config.BaseProxyConf, workConn frpNet.Conn) {

	var (
		remote io.ReadWriteCloser
		err    error
	)
	remote = workConn
	if baseInfo.UseEncryption {
		remote, err = tcp.WithEncryption(remote, []byte(config.ClientCommonCfg.PrivilegeToken))
		if err != nil {
			workConn.Error("create encryption stream error: %v", err)
			return
		}
	}
	if baseInfo.UseCompression {
		remote = tcp.WithCompression(remote)
	}

	if proxyPlugin != nil {
		// if plugin is set, let plugin handle connections first
		workConn.Debug("handle by plugin: %s", proxyPlugin.Name())
		proxyPlugin.Handle(remote)
		workConn.Debug("handle by plugin finished")
		return
	} else {
		localConn, err := frpNet.ConnectTcpServer(fmt.Sprintf("%s:%d", localInfo.LocalIp, localInfo.LocalPort))
		if err != nil {
			workConn.Error("connect to local service [%s:%d] error: %v", localInfo.LocalIp, localInfo.LocalPort, err)
			return
		}

		workConn.Debug("join connections, localConn(l[%s] r[%s]) workConn(l[%s] r[%s])", localConn.LocalAddr().String(),
			localConn.RemoteAddr().String(), workConn.LocalAddr().String(), workConn.RemoteAddr().String())
		tcp.Join(localConn, remote)
		workConn.Debug("join connections closed")
	}
}
