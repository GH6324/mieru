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
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package protocolv2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/enfein/mieru/pkg/mathext"
	"github.com/enfein/mieru/pkg/netutil"
	"github.com/enfein/mieru/pkg/stderror"
)

const (
	segmentTreeCapacity = 4096
	segmentChanCapacity = 256
)

var (
	segmentPollInterval = 10 * time.Millisecond
)

type sessionState int

const (
	sessionInit sessionState = iota
	sessionAttached
	sessionOpening
	sessionEstablished
	sessionClosing
	sessionClosed
)

type Session struct {
	conn Underlay // underlay connection

	id       uint32        // session ID number
	isClient bool          // if this session is owned by client
	mtu      int           // L2 maxinum transmission unit
	state    sessionState  // session state
	done     chan struct{} // indicate the session is complete

	sendQueue *segmentTree  // segments waiting to send
	sendBuf   *segmentTree  // segments sent but not acknowledged
	recvBuf   *segmentTree  // segments received but acknowledge is not sent
	recvQueue *segmentTree  // segments waiting to be read by application
	recvChan  chan *segment // channel to receive segment from underlay

	nextSeq   uint32 // next sequence number to send a segment
	unackSeq  uint32 // unacknowledged sequence number
	unreadBuf []byte // payload removed from the recvQueue that haven't been read by application

	wg    sync.WaitGroup
	rLock sync.Mutex
	wLock sync.Mutex
}

// Session must implement net.Conn interface.
var _ net.Conn = &Session{}

// NewSession creates a new session.
func NewSession(id uint32, isClient bool, mtu int) *Session {
	return &Session{
		conn:      nil,
		id:        id,
		isClient:  isClient,
		mtu:       mtu,
		state:     sessionInit,
		done:      make(chan struct{}),
		sendQueue: newSegmentTree(segmentTreeCapacity),
		sendBuf:   newSegmentTree(segmentTreeCapacity),
		recvBuf:   newSegmentTree(segmentTreeCapacity),
		recvQueue: newSegmentTree(segmentTreeCapacity),
		recvChan:  make(chan *segment, segmentChanCapacity),
	}
}

// Read lets a user to read data from receive queue.
// The data boundary is preserved, i.e. no fragment read.
func (s *Session) Read(b []byte) (n int, err error) {
	s.rLock.Lock()
	defer s.rLock.Unlock()

	// There are some remaining data that application
	// failed to read last time due to short buffer.
	if len(s.unreadBuf) > 0 {
		if len(b) < len(s.unreadBuf) {
			return 0, io.ErrShortBuffer
		}
		n = copy(b, s.unreadBuf)
		s.unreadBuf = nil
		return n, nil
	}

	// Read all the fragments of the original message.
	for {
		seg := s.recvQueue.DeleteMinBlocking()
		if len(s.unreadBuf) == 0 {
			s.unreadBuf = seg.payload
		} else {
			s.unreadBuf = append(s.unreadBuf, seg.payload...)
		}

		fragment, err := seg.Fragment()
		if err != nil {
			return 0, fmt.Errorf("Fragment() failed: %w", err)
		}
		if fragment == 0 {
			break
		}
	}

	if len(b) < len(s.unreadBuf) {
		return 0, io.ErrShortBuffer
	}
	n = copy(b, s.unreadBuf)
	s.unreadBuf = nil
	return n, nil
}

// Write stores the data to send queue.
func (s *Session) Write(b []byte) (n int, err error) {
	if len(b) > MaxPDU {
		return 0, io.ErrShortWrite
	}
	s.wLock.Lock()
	defer s.wLock.Unlock()

	nFragment := 1
	fragmentSize := MaxFragmentSize(s.mtu, s.conn.IPVersion(), s.conn.TransportProtocol())
	if len(b) > fragmentSize {
		nFragment = (len(b)-1)/fragmentSize + 1
	}

	ptr := b
	for i := nFragment - 1; i >= 0; i-- {
		partLen := mathext.Min(fragmentSize, len(ptr))
		part := ptr[:partLen]
		seg := &segment{
			metadata: &dataAckStruct{
				sessionID:  s.id,
				seq:        s.nextSeq,
				unAckSeq:   s.unackSeq,
				windowSize: uint16(s.recvBuf.Remaining()),
				fragment:   uint8(i),
				payloadLen: uint16(partLen),
			},
			payload: part,
		}
		s.nextSeq++
		s.sendQueue.InsertBlocking(seg)
		ptr = ptr[partLen:]
	}

	return len(b), nil
}

// Close terminates the session at our end.
func (s *Session) Close() error {
	s.rLock.Lock()
	s.wLock.Lock()
	defer s.rLock.Unlock()
	defer s.wLock.Unlock()
	close(s.done)
	return nil
}

func (s *Session) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Session) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

func (s *Session) SetDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) SetReadDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) SetWriteDeadline(t time.Time) error {
	return stderror.ErrUnsupported
}

func (s *Session) runInputLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		case seg := <-s.recvChan:
			if err := s.input(seg); err != nil {
				return fmt.Errorf("input() failed: %v", err)
			}
		}
	}
}

func (s *Session) runOutputLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		default:
			switch s.conn.TransportProtocol() {
			case netutil.TCPTransport:
				for {
					seg, err := s.sendQueue.DeleteMin()
					if err != nil {
						if errors.Is(err, stderror.ErrEmpty) {
							time.Sleep(segmentPollInterval)
							break
						} else {
							return fmt.Errorf("sendQueue.DeleteMin() failed: %v", err)
						}
					}
					if err := s.output(seg); err != nil {
						return fmt.Errorf("output() failed: %v", err)
					}
				}
			default:
				return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
			}
		}
	}
}

// input reads incoming packets from network and assemble
// them in the receive buffer and receive queue.
func (s *Session) input(seg *segment) error {
	protocol := seg.Protocol()
	if s.isClient {
		if protocol != dataServerToClient && protocol != ackServerToClient && protocol != closeSessionRequest && protocol != closeSessionResponse {
			return stderror.ErrInvalidArgument
		}
	} else {
		if protocol != dataClientToServer && protocol != ackClientToServer && protocol != closeSessionRequest && protocol != closeSessionResponse {
			return stderror.ErrInvalidArgument
		}
	}
	if protocol == dataServerToClient || protocol == dataClientToServer {
		return s.inputData(seg)
	}
	if protocol == ackServerToClient || protocol == ackClientToServer {
		return s.inputAck(seg)
	}
	return nil
}

func (s *Session) inputData(seg *segment) error {
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		// Deliver the segment directly to recvQueue.
		s.recvQueue.InsertBlocking(seg)
		return nil
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
}

func (s *Session) inputAck(seg *segment) error {
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		// Do nothing when receive ACK from TCP protocol.
		return nil
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
}

func (s *Session) output(seg *segment) error {
	switch s.conn.TransportProtocol() {
	case netutil.TCPTransport:
		if err := s.conn.(*TCPUnderlay).writeOneSegment(seg); err != nil {
			return fmt.Errorf("TCPUnderlay.writeOneSegment() failed: %v", err)
		}
	default:
		return fmt.Errorf("unsupported transport protocol %v", s.conn.TransportProtocol())
	}
	return nil
}