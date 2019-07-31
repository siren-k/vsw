//
// Copyright 2017-2019 Nippon Telegraph and Telephone Corporation.
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

package vswitch

import (
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/lagopus/vsw/dpdk"
	"github.com/lagopus/vsw/utils/notifier"
)

const (
	tapModule    = "tap"
	hostifModule = "hostif"
)

// VRF represents Virtual Routing & Forwarding instance.
type VRF struct {
	name     string
	devs     map[VIFIndex]OutputDevice
	router   *router
	tap      *BaseInstance
	hostif   *BaseInstance
	enabled  bool
	index    VRFIndex
	vifIndex VIFIndex
	rd       uint64 // XXX: Do we need this?
	sadb     *SADatabases
	sadbOnce sync.Once
	*RoutingTable
	*PBR
}

type vrfManager struct {
	mutex     sync.Mutex
	byName    map[string]*VRF
	byIndex   [MaxVRF]*VRF
	nextIndex int
	rds       map[uint64]struct{}
	re        *regexp.Regexp
}

var vrfMgr = &vrfManager{
	byName: make(map[string]*VRF),
	rds:    make(map[uint64]struct{}),
	re:     regexp.MustCompile(`^vrf\d+$`),
}

// should be called via assignIndex only
func (vm *vrfManager) findSlot(vrf *VRF, from, to int) bool {
	for i := from; i < to; i++ {
		if vm.byIndex[i] == nil {
			vrf.index = VRFIndex(i)
			vm.byIndex[i] = vrf
			vm.nextIndex = (i + 1) % len(vm.byIndex)
			return true
		}
	}
	return false
}

// should be called with lock held
func (vm *vrfManager) assignIndex(vrf *VRF) bool {
	// try from the nextIndex to the end
	if vm.findSlot(vrf, vm.nextIndex, len(vm.byIndex)) {
		return true
	}
	// try from the head to the nextIndex
	return vm.findSlot(vrf, 0, vm.nextIndex)
}

// should be called with lock held
func (vm *vrfManager) releaseIndex(vrf *VRF) {
	vm.byIndex[int(vrf.index)] = nil
}

// NewVRF creates a VRF instance.
func NewVRF(name string) (*VRF, error) {
	if !vrfMgr.re.MatchString(name) {
		return nil, fmt.Errorf("Invalid VRF name: '%v'", name)
	}

	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	if _, exists := vrfMgr.byName[name]; exists {
		return nil, fmt.Errorf("VRF %s already exists", name)
	}

	vrf := &VRF{
		name:    name,
		enabled: false,
	}

	if !vrfMgr.assignIndex(vrf) {
		return nil, fmt.Errorf("No space left for new VRF")
	}

	vifIndex, err := vifIdxMgr.allocVIFIndex(vrf)
	if err != nil {
		vrfMgr.releaseIndex(vrf)
		return nil, fmt.Errorf("Can't assign VIFIndex: %v", err)
	}
	vrf.vifIndex = vifIndex

	// Create an ICMP processor
	var errMsg error
	tapName := name + "-tap"
	if tap, err := newInstance(tapModule, tapName, name); err != nil {
		errMsg = fmt.Errorf("ICMP handler instance creation failed: %v", err)
		goto error1
	} else {
		vrf.tap = tap
	}

	// Craete a router
	if router, err := newRouter(vrf, name); err != nil {
		errMsg = fmt.Errorf("Router instance creation failed: %v", err)
		goto error2
	} else {
		vrf.router = router
	}

	// Forward all IP packets to the ICMP processor
	if err := vrf.router.connect(vrf.tap.Input(), MatchIPv4DstSelf, nil); err != nil {
		errMsg = errors.New("Can't connect a router and an tap modules")
		goto error3
	}

	vrf.RoutingTable = newRoutingTable(vrf)
	vrf.devs = make(map[VIFIndex]OutputDevice)
	vrf.PBR = newPBR(vrf)
	vrfMgr.byName[name] = vrf

	noti.Notify(notifier.Add, vrf, nil)

	return vrf, nil

error3:
	vrf.router.free()
error2:
	vrf.tap.free()
error1:
	vrfMgr.releaseIndex(vrf)
	return nil, errMsg
}

func (v *VRF) Free() {
	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	for _, dev := range v.devs {
		if vif, ok := dev.(*VIF); ok {
			v.DeleteVIF(vif)
		}
	}

	v.router.disconnect(MatchIPv4DstSelf, nil)
	v.tap.free()
	v.router.free()
	delete(vrfMgr.byName, v.name)
	vrfMgr.releaseIndex(v)

	if err := vifIdxMgr.freeVIFIndex(v.vifIndex); err != nil {
		logger.Err("Freeing VIFIndex for %v failed: %v", v.name, err)
	}

	if v.rd != 0 {
		delete(vrfMgr.rds, v.rd)
	}

	noti.Notify(notifier.Delete, v, nil)
}

func (v *VRF) baseInstance() *BaseInstance {
	return v.router.base
}

func (v *VRF) IsEnabled() bool {
	return v.enabled
}

func (v *VRF) Enable() error {
	if !v.enabled {
		if err := v.tap.enable(); err != nil {
			return err
		}
		if err := v.router.enable(); err != nil {
			v.tap.disable()
			return err
		}
		v.enabled = true
	}
	return nil
}

func (v *VRF) Disable() {
	if v.enabled {
		v.router.disable()
		v.tap.disable()
		v.enabled = false
	}
}

// Name returns the name of the VRF.
func (v *VRF) Name() string {
	return v.name
}

func (v *VRF) String() string {
	return v.name
}

// Index returns a unique identifier of the VRF.
func (v *VRF) Index() VRFIndex {
	return v.index
}

// VRFIndex returns a unique VIFIndex of the VRF.
// VIFIndex is used for inter-VRF routing.
func (v *VRF) VIFIndex() VIFIndex {
	return v.vifIndex
}

// Input returns an input ring for the VRF
// which is the input ring for the underlying interface.
func (v *VRF) Input() *dpdk.Ring {
	return v.router.base.Input()
}

// SetRD sets the route distinguisher of thr VRF.
func (v *VRF) SetRD(rd uint64) error {
	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	oldrd := v.rd

	if _, exists := vrfMgr.rds[rd]; exists {
		return fmt.Errorf("VRF RD %d already exists", rd)
	}

	v.rd = rd
	vrfMgr.rds[rd] = struct{}{}

	if oldrd != 0 {
		delete(vrfMgr.rds, oldrd)
	}

	return nil
}

// RD returns the route distinguisher of the VRF.
func (v *VRF) RD() uint64 {
	return v.rd
}

// AddVIF adds VIF to the VRF.
// If the same VIF is added more than once to the VRF,
// it sliently ignores.
func (v *VRF) AddVIF(vif *VIF) error {
	var err error

	if _, exists := v.devs[vif.VIFIndex()]; exists {
		return nil
	}

	if err = vif.setVRF(v); err != nil {
		return err
	}

	// router -> VIF
	if err = v.router.addVIF(vif); err != nil {
		goto error1
	}

	// ICMP -> VIF (If not Tunnel)
	if vif.Tunnel() == nil {
		if err = v.tap.connect(vif.Outbound(), MatchOutVIF, vif); err != nil {
			goto error2
		}
	}

	// VIF -> router (DST_SELF)
	if err = vif.connect(v.router.input(), MatchEthDstSelf, nil); err != nil {
		goto error3
	}

	// VIF -> router (broadcast)
	if err = vif.connect(v.router.input(), MatchEthDstBC, nil); err != nil {
		goto error4
	}

	// VIF -> router (multicast)
	if err = vif.connect(v.router.input(), MatchEthDstMC, nil); err != nil {
		goto error5
	}

	// Enable NAPT if needed
	if vif.isNAPTEnabled() {
		if err = v.enableNAPT(vif); err != nil {
			goto error6
		}
	}

	v.devs[vif.VIFIndex()] = vif

	// TUN/TAP for the VIF will be created
	noti.Notify(notifier.Add, v, vif)

	return nil

error6:
	vif.disconnect(MatchEthDstMC, nil)
error5:
	vif.disconnect(MatchEthDstBC, nil)
error4:
	vif.disconnect(MatchEthDstSelf, nil)
error3:
	v.tap.disconnect(MatchOutVIF, vif)
error2:
	v.router.deleteVIF(vif)
error1:
	vif.setVRF(nil)
	return err
}

func (v *VRF) DeleteVIF(vif *VIF) error {
	if _, ok := v.devs[vif.VIFIndex()]; !ok {
		return fmt.Errorf("Can't find %v in the VRF.", vif)
	}

	v.tap.disconnect(MatchOutVIF, vif)
	vif.disconnect(MatchEthDstSelf, nil)
	vif.disconnect(MatchEthDstBC, nil)
	vif.disconnect(MatchEthDstMC, nil)
	vif.setVRF(nil)
	v.router.deleteVIF(vif)

	delete(v.devs, vif.VIFIndex())

	//  TUN/TAP for the VIF will be deleted
	noti.Notify(notifier.Delete, v, vif)

	return nil
}

// VIF returns a slice of Vif Indices in the VRF.
func (v *VRF) VIF() []*VIF {
	var vifs []*VIF
	for _, dev := range v.devs {
		if vif, ok := dev.(*VIF); ok {
			vifs = append(vifs, vif)
		}
	}
	return vifs
}

// Dump returns descriptive information about the VRF
func (v *VRF) Dump() string {
	str := fmt.Sprintf("%s: RD=%d. %d DEV(s):", v.name, v.rd, len(v.devs))
	for _, dev := range v.devs {
		str += fmt.Sprintf(" %v", dev)
	}
	if v.sadb != nil {
		sad := v.sadb.SAD()
		str += fmt.Sprintf("\n%d SAD", len(sad))
		for _, sa := range sad {
			str += fmt.Sprintf("\n\t%v", sa)
		}

		spd := v.sadb.SPD()
		str += fmt.Sprintf("\n%d SPD", len(spd))
		for _, sp := range spd {
			str += fmt.Sprintf("\n\t%v", sp)
		}
	}
	return str
}

// SADatabases returns SADatabases associated with the VRF.
func (v *VRF) SADatabases() *SADatabases {
	v.sadbOnce.Do(func() {
		v.sadb = newSADatabases(v)
	})
	return v.sadb
}

// HasSADatabases returns true if the VRF has associated SADatbases.
// Returns false otherwise.
func (v *VRF) HasSADatabases() bool {
	return v.sadb != nil
}

func (v *VRF) addL3Tunnel(vif *VIF) error {
	t := vif.Tunnel()
	if t == nil {
		return fmt.Errorf("%v is not tunnel.", vif)
	}

	ra := t.RemoteAddresses()
	if len(ra) == 0 {
		return fmt.Errorf("No remote address(es) specified: %v.", t)
	}

	if err := vif.connect(v.router.input(), MatchIPv4Dst, &ra[0]); err != nil {
		return fmt.Errorf("Adding a rule to %v failed for L3 tunnel: %v", vif, err)
	}

	// Forward inbound packets to L3 Tunnel
	local := t.LocalAddress()
	for _, remote := range ra {
		ft := NewFiveTuple()
		ft.DstIP = CreateIPAddr(local)
		ft.SrcIP = CreateIPAddr(remote)
		ft.Proto = t.IPProto()

		if err := v.router.connect(vif.Inbound(), Match5Tuple, ft); err != nil {
			vif.disconnect(MatchIPv4Dst, remote)
			return fmt.Errorf("Adding a rule to router for L3 tunnel failed: %v", err)
		}

		// Add a rule for NAT Traversal, if the tunnel is IPSec.
		if t.Security() == SecurityIPSec {
			nat := NewFiveTuple()
			nat.SrcIP = ft.SrcIP
			nat.DstIP = ft.DstIP
			nat.DstPort = PortRange{Start: 4500}
			nat.Proto = IPP_UDP

			if err := v.router.connect(vif.Inbound(), Match5Tuple, nat); err != nil {
				vif.disconnect(MatchIPv4Dst, remote)
				v.router.disconnect(Match5Tuple, ft)
				return fmt.Errorf("Adding a rule for IPSec NAT traversal failed: %v", err)
			}
		}
	}

	return nil
}

// TODO: Implement delete Tunnel
func (v *VRF) deleteL3Tunnel(vif *VIF) {
	logger.Warning("Deleting L3 Tunnel (%v) from VRF not supported", vif)
}

func (v *VRF) addL2Tunnel(i *Interface) error {
	t := i.Tunnel()
	if t == nil {
		return fmt.Errorf("%v is not tunnel.", i)
	}

	ra := t.RemoteAddresses()
	if len(ra) == 0 {
		return fmt.Errorf("No remote address(es) specified: %v.", t)
	}

	if err := i.connect(v.router.input(), MatchIPv4Dst, &ra[0]); err != nil {
		return fmt.Errorf("Adding a rule to %v failed for L2 tunnel: %v", i, err)
	}

	// Forward inbound packets to L2 Tunnel
	switch e := t.EncapsMethod(); e {
	case EncapsMethodGRE:
		for _, remote := range ra {
			ft := NewFiveTuple()
			ft.SrcIP = CreateIPAddr(remote)
			ft.DstIP = CreateIPAddr(t.LocalAddress())
			ft.Proto = IPP_GRE

			// TODO: We may want to roll back on error.
			if err := v.router.connect(i.Inbound(), Match5Tuple, ft); err != nil {
				logger.Fatalf("Can't connect L2 tunnel to the router: %v", err)
			}
		}

	case EncapsMethodVxLAN:
		for _, remote := range ra {
			vxlan := &VxLAN{
				Src:     remote,
				Dst:     t.LocalAddress(),
				DstPort: t.VxLANPort(),
				VNI:     t.VNI(),
			}

			// TODO: We may want to roll back on error.
			if err := v.router.connect(i.Inbound(), MatchVxLAN, vxlan); err != nil {
				logger.Fatalf("Can't connect L2 tunnel to the router: %v", err)
			}
		}

	default:
		return fmt.Errorf("Unsupported L2 Tunnel encaps method: %v", e)
	}

	return nil
}

// TODO: Implement delete Tunnel
func (v *VRF) deleteL2Tunnel(i *Interface) {
	logger.Warning("Deleting L2 Tunnel (%v) from VRF not supported", i)
}

func (v *VRF) enableNAPT(vif *VIF) error {
	return v.router.enableNAPT(vif)
}

func (v *VRF) disableNAPT(vif *VIF) error {
	return v.router.disableNAPT(vif)
}

func (v *VRF) MarshalJSON() ([]byte, error) {
	return []byte(`"` + v.name + `"`), nil
}

func (v *VRF) registerOutputDevice(dev OutputDevice) error {
	// If OutputDevice is already in devs, it means the OutputDevice
	// is either VIF, or VRF that has already been added.
	if _, exists := v.devs[dev.VIFIndex()]; exists {
		return nil
	}

	// OutputDevice to be added shall be VRF.
	// If OutputDevice is VIF, it should have been added via AddVIF already.
	if _, ok := dev.(*VRF); !ok {
		return fmt.Errorf("OutputDevice is not VRF: %v", dev)
	}

	// Add VRF to router instance
	if err := v.router.addOutputDevice(dev); err != nil {
		return fmt.Errorf("Adding OutputDevice %v failed.", dev)
	}

	v.devs[dev.VIFIndex()] = dev

	return nil
}

func (v *VRF) routeEntryAdded(entry Route) {
	// Check if all OutputDevice that appears in Route has already
	// been registered to the router instance.
	if len(entry.Nexthops) == 0 {
		if err := v.registerOutputDevice(entry.Dev); err != nil {
			logger.Err("%v", err)
			return
		}
	} else {
		for _, nh := range entry.Nexthops {
			if err := v.registerOutputDevice(nh.Dev); err != nil {
				logger.Err("%v", err)
				return
			}
		}
	}

	noti.Notify(notifier.Add, v, entry)
}

func (v *VRF) routeEntryDeleted(entry Route) {
	// TODO: Remove unused VRF from the router instance
	noti.Notify(notifier.Delete, v, entry)
}

func (v *VRF) pbrEntryAdded(entry PBREntry) {
	for _, nh := range entry.NextHops {
		if nh.Dev == nil {
			continue
		}
		if err := v.registerOutputDevice(nh.Dev); err != nil {
			logger.Err("%v", err)
			return
		}
	}

	noti.Notify(notifier.Add, v, entry)
}

func (v *VRF) pbrEntryDeleted(entry PBREntry) {
	// TODO: Remove unused VRF from the router instance
	noti.Notify(notifier.Delete, v, entry)
}

// GetAllVRF returns a slice of available VRF.
func GetAllVRF() []*VRF {
	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	v := make([]*VRF, len(vrfMgr.byName))
	n := 0
	for _, vrf := range vrfMgr.byName {
		v[n] = vrf
		n++
	}
	return v
}

// GetVRFByName returns a VRF with the given name.
func GetVRFByName(name string) *VRF {
	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	return vrfMgr.byName[name]
}

// GetVRFByIndex returns a VRF with the given index.
func GetVRFByIndex(index VRFIndex) *VRF {
	vrfMgr.mutex.Lock()
	defer vrfMgr.mutex.Unlock()

	return vrfMgr.byIndex[int(index)]
}
