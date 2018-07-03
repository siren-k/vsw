//
// Copyright 2017 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package pfkey

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/lagopus/vsw/agents/tunnel/ipsec/connections"
	"github.com/lagopus/vsw/agents/tunnel/ipsec/pfkey"
	"github.com/lagopus/vsw/agents/tunnel/ipsec/pfkey/receiver"
	"github.com/lagopus/vsw/vswitch"
)

const (
	// SockPath path to unix domain socket.
	SockPath = "/var/tmp"
	// SockFile Name of unix domain socket.
	SockFile = SockPath + "/lagopus-%v.sock"
)

// Handler PFKey handler.
// Nolock
type Handler struct {
	name     string
	vrf      *vswitch.VRF
	sock     string
	listener *net.UnixListener
	conns    *conns
	wg       *sync.WaitGroup
	running  bool
}

// NewHandler Create PFKey handler.
func NewHandler(vrf *vswitch.VRF) *Handler {
	return &Handler{
		name:  vrf.Name(),
		vrf:   vrf,
		conns: newConns(),
		wg:    &sync.WaitGroup{},
	}
}

func (h *Handler) clean() {
	os.Remove(h.sock)
}

func (h *Handler) handlePFkeyConn(c net.Conn) error {
	defer h.wg.Done()

	conn := connections.NewConnection(c)
	h.conns.add(conn)

	defer h.conns.closeAndDelete(conn)

	msgMux := receiver.NewMsgMuxForVRF(h.vrf.Index())
	defer msgMux.Free()
	for h.running == true {
		_, err := pfkey.HandlePfkey(c, conn, msgMux.MsgMux)
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("%v: error %v", h, err)
			}
			return err
		}
	}
	return nil
}

func (h *Handler) mainLoop() {
	defer h.wg.Done()
	// for graceful shutdown.
	defer h.conns.allClose()

	for h.running == true {
		if conn, err := h.listener.Accept(); err == nil {
			log.Printf("%v: accept %v", h, conn)
			h.wg.Add(1)
			go h.handlePFkeyConn(conn)
		} else {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("%v: error %v", h, err)
			}
		}
	}
}

// public.

// Start Start PHKey handler.
func (h *Handler) Start() error {
	if h.running {
		return nil
	}

	h.sock = fmt.Sprintf(SockFile, h.vrf.Name())
	h.clean()

	log.Printf("%v: Start pfkey handler: %v", h, h.sock)

	var err error
	if h.listener, err = net.ListenUnix("unixpacket",
		&net.UnixAddr{Name: h.sock, Net: "unixpacket"}); err != nil {
		log.Printf("%v: Can't create listener", h)
		return err
	}

	h.running = true

	h.wg.Add(1)
	go h.mainLoop()

	return nil
}

// Stop Stop PHKey handler.
func (h *Handler) Stop() {
	if !h.running {
		return
	}

	defer h.clean()

	log.Printf("%v: Stop pfkey handler", h)

	h.running = false
	// for graceful shutdown.
	h.listener.Close()
	h.wg.Wait()
}

// String Return Name.
func (h *Handler) String() string {
	return h.name
}
