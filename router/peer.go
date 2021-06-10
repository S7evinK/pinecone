// Copyright 2021 The Matrix.org Foundation C.I.C.
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

package router

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/matrix-org/pinecone/types"
	"github.com/matrix-org/pinecone/util"
	"go.uber.org/atomic"
)

const (
	PeerTypeMulticast int = iota
	PeerTypeBluetooth
	PeerTypeRemote
)

type Peer struct {
	r            *Router                   //
	port         types.SwitchPortID        //
	started      atomic.Bool               // worker goroutines started?
	alive        atomic.Bool               // have we received a handshake?
	mutex        sync.RWMutex              // protects everything below this line
	zone         string                    //
	peertype     int                       //
	context      context.Context           //
	cancel       context.CancelFunc        //
	conn         util.BufferedRWC          // underlying connection to peer
	public       types.PublicKey           //
	trafficOut   *lifoQueue                // queue traffic message to peer
	protoOut     *fifoQueue                // queue protocol message to peer
	coords       types.SwitchPorts         //
	announce     chan *types.Frame         //
	announcement *rootAnnouncementWithTime //
	statistics   peerStatistics            //
}

type peerStatistics struct {
	txProtoSuccessful      atomic.Uint64
	txProtoDropped         atomic.Uint64
	txTrafficSuccessful    atomic.Uint64
	txTrafficDropped       atomic.Uint64
	rxDroppedNoDestination atomic.Uint64
}

func (s *peerStatistics) reset() {
	s.txProtoSuccessful.Store(0)
	s.txProtoDropped.Store(0)
	s.txTrafficSuccessful.Store(0)
	s.txTrafficDropped.Store(0)
	s.rxDroppedNoDestination.Store(0)
}

func (p *Peer) PublicKey() types.PublicKey {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.public
}

func (p *Peer) Coordinates() types.SwitchPorts {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.coords
}

func (p *Peer) SeenCommonRootRecently() bool {
	if !p.alive.Load() {
		return false
	}
	last := p.lastAnnouncement()
	if last == nil {
		return false
	}
	lpk := last.RootPublicKey
	rpk := p.r.RootPublicKey()
	return lpk == rpk
}

func (p *Peer) SeenRecently() bool {
	if last := p.lastAnnouncement(); last != nil {
		return true
	}
	return false
}

func (p *Peer) updateAnnouncement(new *types.SwitchAnnouncement) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	coords, err := new.PeerCoords(p.public)
	if err != nil {
		p.alive.Store(false)
		p.announcement = nil
		p.coords = nil
		return fmt.Errorf("new.PeerCoords: %w", err)
	}
	p.alive.Store(true)
	p.announcement = &rootAnnouncementWithTime{
		SwitchAnnouncement: *new,
		at:                 time.Now(),
	}
	p.coords = coords
	return nil
}

func (p *Peer) lastAnnouncement() *rootAnnouncementWithTime {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	switch {
	case !p.started.Load():
		return nil
	case p.announcement != nil && time.Since(p.announcement.at) >= announcementTimeout:
		return nil
	}
	return p.announcement
}

func (p *Peer) start() error {
	if !p.started.CAS(false, true) {
		return errors.New("switch peer is already started")
	}
	p.alive.Store(false)
	go p.reader()
	go p.writer()
	go p.announcer()
	if ann := p.generateAnnouncement(); ann != nil {
		p.announce <- ann
	}
	return nil
}

func (p *Peer) stop() error {
	if !p.started.CAS(true, false) {
		return errors.New("switch peer is already stopped")
	}
	p.alive.Store(false)
	p.cancel()
	_ = p.conn.Close()
	return nil
}

func (p *Peer) generateAnnouncement() *types.Frame {
	if p.port == 0 {
		return nil
	}
	announcement := p.r.tree.Root()
	for _, sig := range announcement.Signatures {
		if p.r.public.EqualTo(sig.PublicKey) {
			// For some reason the announcement that we want to send already
			// includes our signature. This shouldn't really happen but if we
			// did send it, other nodes would end up ignoring the announcement
			// anyway since it would appear to be a routing loop.
			return nil
		}
	}
	// Sign the announcement.
	if err := announcement.Sign(p.r.private[:], p.port); err != nil {
		p.r.log.Println("Failed to sign switch announcement:", err)
		return nil
	}
	var payload [MaxPayloadSize]byte
	n, err := announcement.MarshalBinary(payload[:])
	if err != nil {
		p.r.log.Println("Failed to marshal switch announcement:", err)
		return nil
	}
	frame := types.GetFrame()
	frame.Version = types.Version0
	frame.Type = types.TypeSTP
	frame.Destination = types.SwitchPorts{}
	frame.Payload = payload[:n]
	return frame
}

func (p *Peer) announcer() {
	p.r.tree.advertise.L.Lock()
	defer p.r.tree.advertise.L.Unlock()
	for {
		select {
		case <-p.context.Done():
			// The switch peer is shutting down.
			return

		default:
			p.r.tree.advertise.Wait()
			if ann := p.generateAnnouncement(); ann != nil {
				p.announce <- ann
			}
		}
	}
}

func (p *Peer) reader() {
	buf := make([]byte, MaxFrameSize)
	for {
		select {
		case <-p.context.Done():
			// The switch peer is shutting down.
			return

		default:
			var n int
			header, err := p.conn.Peek(12)
			if err != nil {
				if err != io.EOF {
					p.r.log.Println("Failed to peek:", err)
				}
				_ = p.r.Disconnect(p.port, fmt.Errorf("p.conn.Peek: %w", err))
				return
			}
			if !bytes.Equal(header[:4], types.FrameMagicBytes) {
				p.r.log.Println(p.port, "traffic had no magic", types.FrameMagicBytes, "bytes", header, types.FrameType(header[1]))
				_, _ = p.conn.Discard(1)
				continue
			}
			header = header[4:]
			expecting := 0
			switch types.FrameType(header[1]) {
			case types.TypeVirtualSnakeBootstrap:
				payloadLen := int(binary.BigEndian.Uint16(header[2:4]))
				coordsLen := int(binary.BigEndian.Uint16(header[4:6]))
				expecting = 10 + coordsLen + payloadLen + ed25519.PublicKeySize

			case types.TypeVirtualSnakeBootstrapACK:
				payloadLen := int(binary.BigEndian.Uint16(header[2:4]))
				dstLen := int(binary.BigEndian.Uint16(header[4:6]))
				srcLen := int(binary.BigEndian.Uint16(header[6:8]))
				expecting = 12 + dstLen + srcLen + payloadLen + (ed25519.PublicKeySize * 2)

			case types.TypeVirtualSnakeSetup:
				payloadLen := int(binary.BigEndian.Uint16(header[2:4]))
				coordsLen := int(binary.BigEndian.Uint16(header[4:6]))
				expecting = 10 + coordsLen + (ed25519.PublicKeySize * 2) + payloadLen

			case types.TypeVirtualSnakeTeardown:
				payloadLen := int(binary.BigEndian.Uint16(header[2:4]))
				expecting = 8 + payloadLen + ed25519.PublicKeySize

			case types.TypeVirtualSnake, types.TypeVirtualSnakePathfind:
				payloadLen := int(binary.BigEndian.Uint16(header[2:4]))
				expecting = 8 + payloadLen + (ed25519.PublicKeySize * 2)

			default:
				dstLen := int(binary.BigEndian.Uint16(header[2:4]))
				srcLen := int(binary.BigEndian.Uint16(header[4:6]))
				payloadLen := int(binary.BigEndian.Uint16(header[6:8]))
				expecting = 12 + dstLen + srcLen + payloadLen
			}
			n, err = io.ReadFull(p.conn, buf[:expecting])
			switch err {
			case io.EOF, io.ErrUnexpectedEOF:
				_ = p.r.Disconnect(p.port, fmt.Errorf("io.ReadFull: %w", err))
				return
			case nil:
			default:
				p.r.log.Println("Failed to read:", err)
				continue
			}
			if n < expecting {
				p.r.log.Println("Expecting", expecting, "bytes but got", n, "bytes")
				continue
			}
			frame := types.GetFrame()
			if _, err := frame.UnmarshalBinary(buf[:n]); err != nil {
				p.r.log.Println("Port", p.port, "error unmarshalling frame:", err)
				frame.Done()
				return
			}
			if frame.Version != types.Version0 {
				p.r.log.Println("Port", p.port, "incorrect version in frame")
				frame.Done()
				return
			}
			func(frame *types.Frame) {
				defer frame.Done()

				//p.r.log.Println("Frame type", frame.Type.String(), frame.DestinationKey)
				sent := false
				defer func() {
					if !sent {
						p.statistics.rxDroppedNoDestination.Inc()
					}
				}()
				for _, port := range p.getNextHops(frame, p.port) {
					// Ignore ports that are not good candidates.
					dest := p.r.ports[port]
					if !dest.started.Load() || (dest.port != 0 && !dest.alive.Load()) {
						continue
					}
					if p.port != 0 && dest.port != 0 {
						if p.port == dest.port || p.public.EqualTo(dest.public) {
							continue
						}
					}
					switch frame.Type {
					case types.TypePathfind, types.TypeVirtualSnakePathfind:
						signedframe, err := p.r.signPathfind(frame, p, dest)
						if err != nil {
							p.r.log.Println("WARNING: Failed to sign pathfind:", err)
							continue
						}
						if signedframe == nil {
							continue
						}
						if sent = dest.trafficOut.push(signedframe); sent {
							dest.statistics.txTrafficSuccessful.Inc()
							return
						} else {
							p.r.log.Println("Dropped pathfind frame of type", signedframe.Type.String(), "on port", dest.port)
							dest.statistics.txTrafficDropped.Inc()
							signedframe.Done()
							continue
						}

					case types.TypeDHTRequest, types.TypeDHTResponse, types.TypeVirtualSnakeBootstrap, types.TypeVirtualSnakeBootstrapACK, types.TypeVirtualSnakeSetup, types.TypeVirtualSnakeTeardown:
						if sent = dest.protoOut.push(frame.Borrow()); sent {
							dest.statistics.txProtoSuccessful.Inc()
							return
						} else {
							p.r.log.Println("Dropped protocol frame of type", frame.Type.String(), "on port", dest.port)
							dest.statistics.txProtoDropped.Inc()
							frame.Done()
							continue
						}

					case types.TypeGreedy, types.TypeSource, types.TypeVirtualSnake:
						if sent = dest.trafficOut.push(frame.Borrow()); sent {
							dest.statistics.txTrafficSuccessful.Inc()
							return
						} else {
							p.r.log.Println("Dropped traffic frame of type", frame.Type.String(), "on port", dest.port)
							dest.statistics.txTrafficDropped.Inc()
							frame.Done()
							continue
						}
					}
				}
			}(frame)
		}
	}
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, MaxFrameSize)
	},
}

func (p *Peer) writer() {
	send := func(frame *types.Frame) error {
		if frame == nil {
			return nil
		}
		buf := bufPool.Get().([]byte)
		defer bufPool.Put(buf) // nolint:staticcheck
		fn, err := frame.MarshalBinary(buf)
		frame.Done()
		if err != nil {
			p.r.log.Println("Port", p.port, "error marshalling frame:", err)
			return err
		}
		if !bytes.Equal(buf[:4], types.FrameMagicBytes) {
			panic("expected magic bytes")
		}
		remaining := buf[:fn]
		for len(remaining) > 0 {
			n, err := p.conn.Write(remaining)
			if err != nil {
				if err != io.EOF {
					p.r.log.Println("Failed to write:", err)
				}
				_ = p.r.Disconnect(p.port, fmt.Errorf("p.conn.Write: %w", err))
				return err
			}
			remaining = remaining[n:]
		}
		p.conn.Flush()
		return nil
	}

	for {
		select {
		case <-p.context.Done():
			return
		case ann := <-p.announce:
			_ = send(ann)
			continue
		default:
		}
		select {
		case <-p.context.Done():
			return
		case ann := <-p.announce:
			_ = send(ann)
			continue
		case <-p.protoOut.wait():
			if frame, ok := p.protoOut.pop(); ok && frame != nil {
				_ = send(frame)
			}
			continue
		default:
		}
		select {
		case <-p.context.Done():
			return
		case ann := <-p.announce:
			_ = send(ann)
			continue
		case <-p.protoOut.wait():
			if frame, ok := p.protoOut.pop(); ok && frame != nil {
				_ = send(frame)
			}
			continue
		case <-p.trafficOut.wait():
			if frame, ok := p.trafficOut.pop(); ok && frame != nil {
				_ = send(frame)
			}
			continue
		}
	}
}

type peers []*Peer

func (p peers) Len() int {
	return len(p)
}

func (p peers) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func (p peers) Less(i, j int) bool {
	p[i].mutex.RLock()
	p[j].mutex.RLock()
	defer p[i].mutex.RUnlock()
	defer p[j].mutex.RUnlock()
	if p[i].peertype < p[j].peertype {
		return true
	}
	return p[i].port < p[j].port
}
