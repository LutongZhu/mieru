// Copyright (C) 2023  mieru authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>

package protocolv2

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/enfein/mieru/pkg/cipher"
	"github.com/enfein/mieru/pkg/log"
	"github.com/enfein/mieru/pkg/netutil"
	"github.com/enfein/mieru/pkg/replay"
	"github.com/enfein/mieru/pkg/rng"
	"github.com/enfein/mieru/pkg/stderror"
)

const (
	udpOverhead          = cipher.DefaultNonceSize + metadataLength + cipher.DefaultOverhead*2
	udpNonHeaderPosition = cipher.DefaultNonceSize + metadataLength + cipher.DefaultOverhead
)

var udpReplayCache = replay.NewCache(16*1024*1024, 2*time.Minute)

type UDPUnderlay struct {
	// ---- common fields ----
	baseUnderlay
	conn  *net.UDPConn
	block cipher.BlockCipher

	// Candidates are block ciphers that can be used to encrypt or decrypt data.
	// When isClient is true, there must be exactly 1 element in the slice.
	candidates []cipher.BlockCipher

	// sendMutex is used when write data to the connection.
	sendMutex sync.Mutex

	// ---- client fields ----
	serverAddr *net.UDPAddr
}

var _ Underlay = &UDPUnderlay{}

// NewUDPUnderlay connects to the remote address "raddr" on the network "udp"
// with packet encryption. If "laddr" is empty, an automatic address is used.
// "block" is the block encryption algorithm to encrypt packets.
func NewUDPUnderlay(ctx context.Context, network, laddr, raddr string, mtu int, block cipher.BlockCipher) (*UDPUnderlay, error) {
	switch network {
	case "udp", "udp4", "udp6":
	default:
		return nil, fmt.Errorf("network %s is not supported for UDP underlay", network)
	}
	if !block.IsStateless() {
		return nil, fmt.Errorf("UDP block cipher must be stateless")
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, fmt.Errorf("net.ResolveUDPAddr() failed: %w", err)
	}
	var localAddr *net.UDPAddr
	if laddr != "" {
		localAddr, err = net.ResolveUDPAddr("udp", laddr)
		if err != nil {
			return nil, fmt.Errorf("net.ResolveUDPAddr() failed: %w", err)
		}
	}

	conn, err := net.ListenUDP(network, localAddr)
	if err != nil {
		return nil, fmt.Errorf("net.ListenUDP() failed: %w", err)
	}
	log.Debugf("Created new client UDP underlay [%v - %v]", conn.LocalAddr(), remoteAddr)
	return &UDPUnderlay{
		baseUnderlay: *newBaseUnderlay(true, mtu),
		conn:         conn,
		serverAddr:   remoteAddr,
		block:        block,
		candidates:   []cipher.BlockCipher{block},
	}, nil
}

func (u *UDPUnderlay) String() string {
	if u.conn == nil {
		return "UDPUnderlay{}"
	}
	if u.isClient {
		return fmt.Sprintf("UDPUnderlay{%v - %v}", u.LocalAddr(), u.RemoteAddr())
	} else {
		return fmt.Sprintf("UDPUnderlay{%v}", u.LocalAddr())
	}
}

func (u *UDPUnderlay) Close() error {
	select {
	case <-u.done:
		return nil
	default:
	}

	log.Debugf("Closing %v", u)
	u.baseUnderlay.Close()
	return u.conn.Close()
}

func (u *UDPUnderlay) IPVersion() netutil.IPVersion {
	if u.conn == nil {
		return netutil.IPVersionUnknown
	}
	return netutil.GetIPVersion(u.conn.LocalAddr().String())
}

func (u *UDPUnderlay) TransportProtocol() netutil.TransportProtocol {
	return netutil.UDPTransport
}

func (u *UDPUnderlay) LocalAddr() net.Addr {
	return u.conn.LocalAddr()
}

func (u *UDPUnderlay) RemoteAddr() net.Addr {
	if u.serverAddr != nil {
		return u.serverAddr
	}
	return netutil.NilNetAddr()
}

func (u *UDPUnderlay) AddSession(s *Session) error {
	if err := u.baseUnderlay.AddSession(s); err != nil {
		return err
	}
	s.conn = u // override base underlay
	close(s.ready)
	log.Debugf("Adding session %d to %v", s.id, u)

	s.wg.Add(2)
	go func() {
		if err := s.runInputLoop(context.Background()); err != nil {
			log.Debugf("%v runInputLoop(): %v", s, err)
		}
		s.wg.Done()
	}()
	go func() {
		if err := s.runOutputLoop(context.Background()); err != nil {
			log.Debugf("%v runOutputLoop(): %v", s, err)
		}
		s.wg.Done()
	}()
	return nil
}

func (u *UDPUnderlay) RemoveSession(s *Session) error {
	err := u.baseUnderlay.RemoveSession(s)
	if len(u.baseUnderlay.sessionMap) == 0 {
		u.Close()
	}
	return err
}

func (u *UDPUnderlay) RunEventLoop(ctx context.Context) error {
	if u.conn == nil {
		return stderror.ErrNullPointer
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-u.done:
			return nil
		default:
		}
		seg, addr, err := u.readOneSegment()
		if err != nil {
			return fmt.Errorf("readOneSegment() failed: %w", err)
		}
		if log.IsLevelEnabled(log.TraceLevel) {
			log.Tracef("%v received one segment: peer = %v, protocol = %d, payload size = %d", u, addr, seg.metadata.Protocol(), len(seg.payload))
		}
		if isSessionProtocol(seg.metadata.Protocol()) {
			switch seg.metadata.Protocol() {
			case openSessionRequest:
				if err := u.onOpenSessionRequest(seg); err != nil {
					return fmt.Errorf("onOpenSessionRequest() failed: %v", err)
				}
			case openSessionResponse:
				if err := u.onOpenSessionResponse(seg); err != nil {
					return fmt.Errorf("onOpenSessionResponse() failed: %v", err)
				}
			case closeSessionRequest, closeSessionResponse:
				if err := u.onCloseSession(seg); err != nil {
					return fmt.Errorf("onCloseSession() failed: %v", err)
				}
			default:
				panic(fmt.Sprintf("Protocol %d is a session protocol but not recognized by UDP underlay", seg.metadata.Protocol()))
			}
		} else if isDataAckProtocol(seg.metadata.Protocol()) {
			das, _ := toDataAckStruct(seg.metadata)
			u.sessionLock.Lock()
			session, ok := u.sessionMap[das.sessionID]
			u.sessionLock.Unlock()
			if !ok {
				log.Debugf("Session %d is not registered to %v", das.sessionID, u)
				continue
			}
			session.recvChan <- seg
		} else if isCloseConnProtocol(seg.metadata.Protocol()) {
			// Close connection.
		}
		// Ignore other protocols.
	}
}

func (u *UDPUnderlay) onOpenSessionRequest(seg *segment) error {
	if u.isClient {
		return stderror.ErrInvalidOperation
	}

	// Create a new session.
	sessionID := seg.metadata.(*sessionStruct).sessionID
	if sessionID == 0 {
		// 0 is reserved and can't be used.
		return fmt.Errorf("reserved session ID %d is used", sessionID)
	}
	u.sessionLock.Lock()
	_, found := u.sessionMap[sessionID]
	u.sessionLock.Unlock()
	if found {
		log.Debugf("%v received open session request, but session ID %d is already used", u, sessionID)
		return nil
	}
	session := NewSession(sessionID, u.isClient, u.MTU())
	u.AddSession(session)
	session.recvChan <- seg
	u.readySessions <- session
	return nil
}

func (u *UDPUnderlay) onOpenSessionResponse(seg *segment) error {
	if !u.isClient {
		return stderror.ErrInvalidOperation
	}

	sessionID := seg.metadata.(*sessionStruct).sessionID
	u.sessionLock.Lock()
	session, found := u.sessionMap[sessionID]
	u.sessionLock.Unlock()
	if !found {
		return fmt.Errorf("session ID %d is not found", sessionID)
	}
	session.recvChan <- seg
	return nil
}

func (u *UDPUnderlay) onCloseSession(seg *segment) error {
	ss := seg.metadata.(*sessionStruct)
	sessionID := ss.sessionID
	u.sessionLock.Lock()
	session, found := u.sessionMap[sessionID]
	u.sessionLock.Unlock()
	if !found {
		log.Debugf("%v received close session request or response, but session ID %d is not found", u, sessionID)
		return nil
	}
	session.recvChan <- seg
	session.wg.Wait()
	u.RemoveSession(session)
	return nil
}

func (u *UDPUnderlay) readOneSegment() (*segment, *net.UDPAddr, error) {
	b := make([]byte, u.mtu)
	var n int
	var addr *net.UDPAddr
	var err error
	for {
		n, addr, err = u.conn.ReadFromUDP(b)
		if err != nil {
			return nil, nil, fmt.Errorf("ReadFromUDP() failed: %w", err)
		}
		if u.isClient && addr.String() != u.serverAddr.String() {
			UnderlayUnsolicitedUDP.Add(1)
			if log.IsLevelEnabled(log.TraceLevel) {
				log.Tracef("%v received unsolicited UDP packet from %v", u, addr)
			}
			continue
		}
		if n < udpOverhead {
			UnderlayMalformedUDP.Add(1)
			if log.IsLevelEnabled(log.TraceLevel) {
				log.Tracef("%v received UDP packet from %v with only %d bytes, which is too short", u, addr, n)
			}
			continue
		}
		b = b[:n]

		// Read encrypted metadata.
		readLen := metadataLength + cipher.DefaultOverhead
		encryptedMeta := b[:readLen]
		if udpReplayCache.IsDuplicate(encryptedMeta[:cipher.DefaultOverhead], addr.String()) {
			replay.NewSession.Add(1)
			return nil, nil, fmt.Errorf("found possible replay attack in %v from %v", u, addr)
		}

		// Decrypt metadata.
		var decryptedMeta []byte
		if u.block == nil && u.isClient {
			u.block = u.candidates[0].Clone()
		}
		if u.block == nil {
			var peerBlock cipher.BlockCipher
			peerBlock, decryptedMeta, err = cipher.SelectDecrypt(encryptedMeta, cipher.CloneBlockCiphers(u.candidates))
			if err != nil {
				UnderlayMalformedUDP.Add(1)
				if log.IsLevelEnabled(log.TraceLevel) {
					log.Tracef("%v cipher.SelectDecrypt() failed with UDP packet from %v", u, addr)
				}
				continue
			}
			u.block = peerBlock.Clone()
		} else {
			decryptedMeta, err = u.block.Decrypt(encryptedMeta)
			if err != nil {
				UnderlayMalformedUDP.Add(1)
				if log.IsLevelEnabled(log.TraceLevel) {
					log.Tracef("%v Decrypt() failed with UDP packet from %v", u, addr)
				}
				continue
			}
		}
		if len(decryptedMeta) != metadataLength {
			return nil, nil, fmt.Errorf("decrypted metadata size %d is unexpected", len(decryptedMeta))
		}

		// Read payload and construct segment.
		var seg *segment
		p := decryptedMeta[0]
		if isSessionProtocol(p) {
			ss := &sessionStruct{}
			if err := ss.Unmarshal(decryptedMeta); err != nil {
				return nil, nil, fmt.Errorf("Unmarshal() to sessionStruct failed: %w", err)
			}
			seg, err = u.readSessionSegment(ss, b[udpNonHeaderPosition:])
			if err != nil {
				return nil, nil, err
			}
			return seg, addr, nil
		} else if isDataAckProtocol(p) {
			das := &dataAckStruct{}
			if err := das.Unmarshal(decryptedMeta); err != nil {
				return nil, nil, fmt.Errorf("Unmarshal() to dataAckStruct failed: %w", err)
			}
			seg, err = u.readDataAckSegment(das, b[udpNonHeaderPosition:])
			if err != nil {
				return nil, nil, err
			}
			return seg, addr, nil
		}

		// TODO: handle close connection
		return nil, nil, fmt.Errorf("unable to handle protocol %d", p)
	}
}

func (u *UDPUnderlay) readSessionSegment(ss *sessionStruct, remaining []byte) (*segment, error) {
	var decryptedPayload []byte
	var err error

	if ss.payloadLen > 0 {
		if len(remaining) < int(ss.payloadLen)+cipher.DefaultOverhead {
			return nil, fmt.Errorf("payload: received incomplete UDP packet")
		}
		decryptedPayload, err = u.block.Decrypt(remaining[:ss.payloadLen+cipher.DefaultOverhead])
		if err != nil {
			return nil, fmt.Errorf("Decrypt() failed: %w", err)
		}
	}
	if int(ss.payloadLen)+cipher.DefaultOverhead+int(ss.suffixLen) != len(remaining) {
		return nil, fmt.Errorf("padding: size not match")
	}

	return &segment{
		metadata: ss,
		payload:  decryptedPayload,
	}, nil
}

func (u *UDPUnderlay) readDataAckSegment(das *dataAckStruct, remaining []byte) (*segment, error) {
	var decryptedPayload []byte
	var err error

	if das.prefixLen > 0 {
		remaining = remaining[das.prefixLen:]
	}
	if das.payloadLen > 0 {
		if len(remaining) < int(das.payloadLen)+cipher.DefaultOverhead {
			return nil, fmt.Errorf("payload: received incomplete UDP packet")
		}
		decryptedPayload, err = u.block.Decrypt(remaining[:das.payloadLen+cipher.DefaultOverhead])
		if err != nil {
			return nil, fmt.Errorf("Decrypt() failed: %w", err)
		}
	}
	if int(das.payloadLen)+cipher.DefaultOverhead+int(das.suffixLen) != len(remaining) {
		return nil, fmt.Errorf("padding: size not match")
	}

	return &segment{
		metadata: das,
		payload:  decryptedPayload,
	}, nil
}

func (u *UDPUnderlay) writeOneSegment(seg *segment, addr *net.UDPAddr) error {
	if seg == nil {
		return stderror.ErrNullPointer
	}
	if u.isClient && addr.String() != u.serverAddr.String() {
		return fmt.Errorf("can't write to %v, UDP server address is %v", addr, u.serverAddr)
	}

	u.sendMutex.Lock()
	defer u.sendMutex.Unlock()

	if u.block == nil {
		return fmt.Errorf("%v cipher block is not ready", u)
	}
	if ss, ok := toSessionStruct(seg.metadata); ok {
		suffixLen := rng.Intn(255)
		ss.suffixLen = uint8(suffixLen)
		padding := newPadding(suffixLen)

		plaintextMetadata := ss.Marshal()
		encryptedMetadata, err := u.block.Encrypt(plaintextMetadata)
		if err != nil {
			return fmt.Errorf("Encrypt() failed: %w", err)
		}
		dataToSend := encryptedMetadata
		if len(seg.payload) > 0 {
			encryptedPayload, err := u.block.Encrypt(seg.payload)
			if err != nil {
				return fmt.Errorf("Encrypt() failed: %w", err)
			}
			dataToSend = append(dataToSend, encryptedPayload...)
		}
		dataToSend = append(dataToSend, padding...)
		if _, err := u.conn.WriteToUDP(dataToSend, addr); err != nil {
			return fmt.Errorf("WriteToUDP() failed: %w", err)
		}
	} else if das, ok := toDataAckStruct(seg.metadata); ok {
		paddingLen1 := rng.Intn(255)
		paddingLen2 := rng.Intn(255)
		das.prefixLen = uint8(paddingLen1)
		das.suffixLen = uint8(paddingLen2)
		padding1 := newPadding(paddingLen1)
		padding2 := newPadding(paddingLen2)

		plaintextMetadata := das.Marshal()
		encryptedMetadata, err := u.block.Encrypt(plaintextMetadata)
		if err != nil {
			return fmt.Errorf("Encrypt() failed: %w", err)
		}
		dataToSend := append(encryptedMetadata, padding1...)
		if len(seg.payload) > 0 {
			encryptedPayload, err := u.block.Encrypt(seg.payload)
			if err != nil {
				return fmt.Errorf("Encrypt() failed: %w", err)
			}
			dataToSend = append(dataToSend, encryptedPayload...)
		}
		dataToSend = append(dataToSend, padding2...)
		if _, err := u.conn.WriteToUDP(dataToSend, addr); err != nil {
			return fmt.Errorf("WriteToUDP() failed: %w", err)
		}
	} else if ccs, ok := toCloseConnStruct(seg.metadata); ok {
		suffixLen := rng.Intn(255)
		ccs.suffixLen = uint8(suffixLen)
		padding := newPadding(suffixLen)

		plaintextMetadata := ccs.Marshal()
		encryptedMetadata, err := u.block.Encrypt(plaintextMetadata)
		if err != nil {
			return fmt.Errorf("Encrypt() failed: %w", err)
		}
		dataToSend := encryptedMetadata
		dataToSend = append(dataToSend, padding...)
		if _, err := u.conn.WriteToUDP(dataToSend, addr); err != nil {
			return fmt.Errorf("WriteToUDP() failed: %w", err)
		}
	} else {
		return stderror.ErrInvalidArgument
	}

	return nil
}