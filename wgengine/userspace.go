// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package wgengine

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/wireguard-go/device"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/exp/maps"
	"tailscale.com/control/controlclient"
	"tailscale.com/envknob"
	"tailscale.com/health"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/dns"
	"tailscale.com/net/dns/resolver"
	"tailscale.com/net/flowtrack"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/packet"
	"tailscale.com/net/tsaddr"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/net/tstun"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/deephash"
	"tailscale.com/util/mak"
	"tailscale.com/wgengine/capture"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/magicsock"
	"tailscale.com/wgengine/monitor"
	"tailscale.com/wgengine/netlog"
	"tailscale.com/wgengine/router"
	"tailscale.com/wgengine/wgcfg"
	"tailscale.com/wgengine/wgint"
	"tailscale.com/wgengine/wglog"
)

// Lazy wireguard-go configuration parameters.
const (
	// lazyPeerIdleThreshold is the idle duration after
	// which we remove a peer from the wireguard configuration.
	// (This includes peers that have never been idle, which
	// effectively have infinite idleness)
	lazyPeerIdleThreshold = 5 * time.Minute

	// packetSendTimeUpdateFrequency controls how often we record
	// the time that we wrote a packet to an IP address.
	packetSendTimeUpdateFrequency = 10 * time.Second

	// packetSendRecheckWireguardThreshold controls how long we can go
	// between packet sends to an IP before checking to see
	// whether this IP address needs to be added back to the
	// WireGuard peer oconfig.
	packetSendRecheckWireguardThreshold = 1 * time.Minute
)

// statusPollInterval is how often we ask wireguard-go for its engine
// status (as long as there's activity). See docs on its use below.
const statusPollInterval = 1 * time.Minute

// networkLoggerUploadTimeout is the maximum timeout to wait when
// shutting down the network logger as it uploads the last network log messages.
const networkLoggerUploadTimeout = 5 * time.Second

type userspaceEngine struct {
	logf              logger.Logf
	wgLogger          *wglog.Logger //a wireguard-go logging wrapper
	reqCh             chan struct{}
	waitCh            chan struct{} // chan is closed when first Close call completes; contrast with closing bool
	timeNow           func() mono.Time
	tundev            *tstun.Wrapper
	wgdev             *device.Device
	router            router.Router
	confListenPort    uint16 // original conf.ListenPort
	dns               *dns.Manager
	magicConn         *magicsock.Conn
	linkMon           *monitor.Mon
	linkMonOwned      bool       // whether we created linkMon (and thus need to close it)
	linkMonUnregister func()     // unsubscribes from changes; used regardless of linkMonOwned
	birdClient        BIRDClient // or nil

	testMaybeReconfigHook func() // for tests; if non-nil, fires if maybeReconfigWireguardLocked called

	// isLocalAddr reports the whether an IP is assigned to the local
	// tunnel interface. It's used to reflect local packets
	// incorrectly sent to us.
	isLocalAddr syncs.AtomicValue[func(netip.Addr) bool]

	// isDNSIPOverTailscale reports the whether a DNS resolver's IP
	// is being routed over Tailscale.
	isDNSIPOverTailscale syncs.AtomicValue[func(netip.Addr) bool]

	wgLock              sync.Mutex // serializes all wgdev operations; see lock order comment below
	lastCfgFull         wgcfg.Config
	lastNMinPeers       int
	lastRouterSig       deephash.Sum // of router.Config
	lastEngineSigFull   deephash.Sum // of full wireguard config
	lastEngineSigTrim   deephash.Sum // of trimmed wireguard config
	lastDNSConfig       *dns.Config
	lastIsSubnetRouter  bool // was the node a primary subnet router in the last run.
	recvActivityAt      map[key.NodePublic]mono.Time
	trimmedNodes        map[key.NodePublic]bool   // set of node keys of peers currently excluded from wireguard config
	sentActivityAt      map[netip.Addr]*mono.Time // value is accessed atomically
	destIPActivityFuncs map[netip.Addr]func()
	statusBufioReader   *bufio.Reader // reusable for UAPI
	lastStatusPollTime  mono.Time     // last time we polled the engine status

	mu                  sync.Mutex         // guards following; see lock order comment below
	netMap              *netmap.NetworkMap // or nil
	closing             bool               // Close was called (even if we're still closing)
	statusCallback      StatusCallback
	peerSequence        []key.NodePublic
	endpoints           []tailcfg.Endpoint
	pendOpen            map[flowtrack.Tuple]*pendingOpenFlow // see pendopen.go
	networkMapCallbacks map[*someHandle]NetworkMapCallback
	tsIPByIPPort        map[netip.AddrPort]netip.Addr // allows registration of IP:ports as belonging to a certain Tailscale IP for whois lookups

	// pongCallback is the map of response handlers waiting for disco or TSMP
	// pong callbacks. The map key is a random slice of bytes.
	pongCallback map[[8]byte]func(packet.TSMPPongReply)
	// icmpEchoResponseCallback is the map of reponse handlers waiting for ICMP
	// echo responses. The map key is a random uint32 that is the little endian
	// value of the ICMP identifer and sequence number concatenated.
	icmpEchoResponseCallback map[uint32]func()

	// networkLogger logs statistics about network connections.
	networkLogger netlog.Logger

	// Lock ordering: magicsock.Conn.mu, wgLock, then mu.
}

// InternalsGetter is implemented by Engines that can export their internals.
type InternalsGetter interface {
	GetInternals() (_ *tstun.Wrapper, _ *magicsock.Conn, _ *dns.Manager, ok bool)
}

func (e *userspaceEngine) GetInternals() (_ *tstun.Wrapper, _ *magicsock.Conn, _ *dns.Manager, ok bool) {
	return e.tundev, e.magicConn, e.dns, true
}

// ResolvingEngine is implemented by Engines that have DNS resolvers.
type ResolvingEngine interface {
	GetResolver() (_ *resolver.Resolver, ok bool)
}

var (
	_ ResolvingEngine = (*userspaceEngine)(nil)
	_ ResolvingEngine = (*watchdogEngine)(nil)
)

func (e *userspaceEngine) GetResolver() (r *resolver.Resolver, ok bool) {
	return e.dns.Resolver(), true
}

// BIRDClient handles communication with the BIRD Internet Routing Daemon.
type BIRDClient interface {
	EnableProtocol(proto string) error
	DisableProtocol(proto string) error
	Close() error
}

// Config is the engine configuration.
type Config struct {
	// Tun is the device used by the Engine to exchange packets with
	// the OS.
	// If nil, a fake Device that does nothing is used.
	Tun tun.Device

	// IsTAP is whether Tun is actually a TAP (Layer 2) device that'll
	// require ethernet headers.
	IsTAP bool

	// Router interfaces the Engine to the OS network stack.
	// If nil, a fake Router that does nothing is used.
	Router router.Router

	// DNS interfaces the Engine to the OS DNS resolver configuration.
	// If nil, a fake OSConfigurator that does nothing is used.
	DNS dns.OSConfigurator

	// LinkMonitor optionally provides an existing link monitor to re-use.
	// If nil, a new link monitor is created.
	LinkMonitor *monitor.Mon

	// Dialer is the dialer to use for outbound connections.
	// If nil, a new Dialer is created
	Dialer *tsdial.Dialer

	// ListenPort is the port on which the engine will listen.
	// If zero, a port is automatically selected.
	ListenPort uint16

	// RespondToPing determines whether this engine should internally
	// reply to ICMP pings, without involving the OS.
	// Used in "fake" mode for development.
	RespondToPing bool

	// BIRDClient, if non-nil, will be used to configure BIRD whenever
	// this node is a primary subnet router.
	BIRDClient BIRDClient
}

func NewFakeUserspaceEngine(logf logger.Logf, listenPort uint16) (Engine, error) {
	logf("Starting userspace WireGuard engine (with fake TUN device)")
	return NewUserspaceEngine(logf, Config{
		ListenPort:    listenPort,
		RespondToPing: true,
	})
}

// NetstackRouterType is a gross cross-package init-time registration
// from netstack to here, informing this package of netstack's router
// type.
var NetstackRouterType reflect.Type

// IsNetstackRouter reports whether e is either fully netstack based
// (without TUN) or is at least using netstack for routing.
func IsNetstackRouter(e Engine) bool {
	switch e := e.(type) {
	case *userspaceEngine:
		if reflect.TypeOf(e.router) == NetstackRouterType {
			return true
		}
	case *watchdogEngine:
		return IsNetstackRouter(e.wrap)
	}
	return IsNetstack(e)
}

// IsNetstack reports whether e is a netstack-based TUN-free engine.
func IsNetstack(e Engine) bool {
	ig, ok := e.(InternalsGetter)
	if !ok {
		return false
	}
	tw, _, _, ok := ig.GetInternals()
	if !ok {
		return false
	}
	name, err := tw.Name()
	return err == nil && name == "FakeTUN"
}

// NewUserspaceEngine creates the named tun device and returns a
// Tailscale Engine running on it.
func NewUserspaceEngine(logf logger.Logf, conf Config) (_ Engine, reterr error) {
	var closePool closeOnErrorPool
	defer closePool.closeAllIfError(&reterr)

	if conf.Tun == nil {
		logf("[v1] using fake (no-op) tun device")
		conf.Tun = tstun.NewFake()
	}
	if conf.Router == nil {
		logf("[v1] using fake (no-op) OS network configurator")
		conf.Router = router.NewFake(logf)
	}
	if conf.DNS == nil {
		logf("[v1] using fake (no-op) DNS configurator")
		d, err := dns.NewNoopManager()
		if err != nil {
			return nil, err
		}
		conf.DNS = d
	}
	if conf.Dialer == nil {
		conf.Dialer = &tsdial.Dialer{Logf: logf}
	}

	var tsTUNDev *tstun.Wrapper
	if conf.IsTAP {
		tsTUNDev = tstun.WrapTAP(logf, conf.Tun)
	} else {
		tsTUNDev = tstun.Wrap(logf, conf.Tun)
	}
	closePool.add(tsTUNDev)

	e := &userspaceEngine{
		timeNow:        mono.Now,
		logf:           logf,
		reqCh:          make(chan struct{}, 1),
		waitCh:         make(chan struct{}),
		tundev:         tsTUNDev,
		router:         conf.Router,
		confListenPort: conf.ListenPort,
		birdClient:     conf.BIRDClient,
	}

	if e.birdClient != nil {
		// Disable the protocol at start time.
		if err := e.birdClient.DisableProtocol("tailscale"); err != nil {
			return nil, err
		}
	}
	e.isLocalAddr.Store(tsaddr.NewContainsIPFunc(nil))
	e.isDNSIPOverTailscale.Store(tsaddr.NewContainsIPFunc(nil))

	if conf.LinkMonitor != nil {
		e.linkMon = conf.LinkMonitor
	} else {
		mon, err := monitor.New(logf)
		if err != nil {
			return nil, err
		}
		closePool.add(mon)
		e.linkMon = mon
		e.linkMonOwned = true
	}

	tunName, _ := conf.Tun.Name()
	conf.Dialer.SetTUNName(tunName)
	conf.Dialer.SetLinkMonitor(e.linkMon)
	e.dns = dns.NewManager(logf, conf.DNS, e.linkMon, conf.Dialer, fwdDNSLinkSelector{e, tunName})

	logf("link state: %+v", e.linkMon.InterfaceState())

	unregisterMonWatch := e.linkMon.RegisterChangeCallback(func(changed bool, st *interfaces.State) {
		tshttpproxy.InvalidateCache()
		e.linkChange(changed, st)
	})
	closePool.addFunc(unregisterMonWatch)
	e.linkMonUnregister = unregisterMonWatch

	endpointsFn := func(endpoints []tailcfg.Endpoint) {
		e.mu.Lock()
		e.endpoints = append(e.endpoints[:0], endpoints...)
		e.mu.Unlock()

		e.RequestStatus()
	}
	magicsockOpts := magicsock.Options{
		Logf:             logf,
		Port:             conf.ListenPort,
		EndpointsFunc:    endpointsFn,
		DERPActiveFunc:   e.RequestStatus,
		IdleFunc:         e.tundev.IdleDuration,
		NoteRecvActivity: e.noteRecvActivity,
		LinkMonitor:      e.linkMon,
	}

	var err error
	e.magicConn, err = magicsock.NewConn(magicsockOpts)
	if err != nil {
		return nil, fmt.Errorf("wgengine: %v", err)
	}
	closePool.add(e.magicConn)
	e.magicConn.SetNetworkUp(e.linkMon.InterfaceState().AnyInterfaceUp())

	tsTUNDev.SetDiscoKey(e.magicConn.DiscoPublicKey())

	if conf.RespondToPing {
		e.tundev.PostFilterIn = echoRespondToAll
	}
	e.tundev.PreFilterFromTunToEngine = e.handleLocalPackets

	if envknob.BoolDefaultTrue("TS_DEBUG_CONNECT_FAILURES") {
		if e.tundev.PreFilterIn != nil {
			return nil, errors.New("unexpected PreFilterIn already set")
		}
		e.tundev.PreFilterIn = e.trackOpenPreFilterIn
		if e.tundev.PostFilterOut != nil {
			return nil, errors.New("unexpected PostFilterOut already set")
		}
		e.tundev.PostFilterOut = e.trackOpenPostFilterOut
	}

	e.wgLogger = wglog.NewLogger(logf)
	e.tundev.OnTSMPPongReceived = func(pong packet.TSMPPongReply) {
		e.mu.Lock()
		defer e.mu.Unlock()
		cb := e.pongCallback[pong.Data]
		e.logf("wgengine: got TSMP pong %02x, peerAPIPort=%v; cb=%v", pong.Data, pong.PeerAPIPort, cb != nil)
		if cb != nil {
			go cb(pong)
		}
	}

	e.tundev.OnICMPEchoResponseReceived = func(p *packet.Parsed) bool {
		idSeq := p.EchoIDSeq()
		e.mu.Lock()
		defer e.mu.Unlock()
		cb := e.icmpEchoResponseCallback[idSeq]
		if cb == nil {
			// We didn't swallow it, so let it flow to the host.
			return false
		}
		e.logf("wgengine: got diagnostic ICMP response %02x", idSeq)
		go cb()
		return true
	}

	// wgdev takes ownership of tundev, will close it when closed.
	e.logf("Creating WireGuard device...")
	e.wgdev = wgcfg.NewDevice(e.tundev, e.magicConn.Bind(), e.wgLogger.DeviceLogger)
	closePool.addFunc(e.wgdev.Close)
	closePool.addFunc(func() {
		if err := e.magicConn.Close(); err != nil {
			e.logf("error closing magicconn: %v", err)
		}
	})

	go func() {
		up := false
		for event := range e.tundev.EventsUpDown() {
			if event&tun.EventUp != 0 && !up {
				e.logf("external route: up")
				e.RequestStatus()
				up = true
			}
			if event&tun.EventDown != 0 && up {
				e.logf("external route: down")
				e.RequestStatus()
				up = false
			}
		}
	}()

	e.logf("Bringing WireGuard device up...")
	if err := e.wgdev.Up(); err != nil {
		return nil, fmt.Errorf("wgdev.Up: %w", err)
	}
	e.logf("Bringing router up...")
	if err := e.router.Up(); err != nil {
		return nil, fmt.Errorf("router.Up: %w", err)
	}

	// It's a little pointless to apply no-op settings here (they
	// should already be empty?), but it at least exercises the
	// router implementation early on.
	e.logf("Clearing router settings...")
	if err := e.router.Set(nil); err != nil {
		return nil, fmt.Errorf("router.Set(nil): %w", err)
	}
	e.logf("Starting link monitor...")
	e.linkMon.Start()

	e.logf("Engine created.")
	return e, nil
}

// echoRespondToAll is an inbound post-filter responding to all echo requests.
func echoRespondToAll(p *packet.Parsed, t *tstun.Wrapper) filter.Response {
	if p.IsEchoRequest() {
		header := p.ICMP4Header()
		header.ToResponse()
		outp := packet.Generate(&header, p.Payload())
		t.InjectOutbound(outp)
		// We already responded to it, but it's not an error.
		// Proceed with regular delivery. (Since this code is only
		// used in fake mode, regular delivery just means throwing
		// it away. If this ever gets run in non-fake mode, you'll
		// get double responses to pings, which is an indicator you
		// shouldn't be doing that I guess.)
		return filter.Accept
	}
	return filter.Accept
}

// handleLocalPackets inspects packets coming from the local network
// stack, and intercepts any packets that should be handled by
// tailscaled directly. Other packets are allowed to proceed into the
// main ACL filter.
func (e *userspaceEngine) handleLocalPackets(p *packet.Parsed, t *tstun.Wrapper) filter.Response {
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		isLocalAddr, ok := e.isLocalAddr.LoadOk()
		if !ok {
			e.logf("[unexpected] e.isLocalAddr was nil, can't check for loopback packet")
		} else if isLocalAddr(p.Dst.Addr()) {
			// macOS NetworkExtension directs packets destined to the
			// tunnel's local IP address into the tunnel, instead of
			// looping back within the kernel network stack. We have to
			// notice that an outbound packet is actually destined for
			// ourselves, and loop it back into macOS.
			t.InjectInboundCopy(p.Buffer())
			metricReflectToOS.Add(1)
			return filter.Drop
		}
	}

	return filter.Accept
}

var debugTrimWireguard = envknob.RegisterOptBool("TS_DEBUG_TRIM_WIREGUARD")

// forceFullWireguardConfig reports whether we should give wireguard our full
// network map, even for inactive peers.
//
// TODO(bradfitz): remove this at some point. We had a TODO to do it before 1.0
// but it's still there as of 1.30. Really we should not do this wireguard lazy
// peer config at all and just fix wireguard-go to not have so much extra memory
// usage per peer. That would simplify a lot of Tailscale code. OTOH, we have 50
// MB of memory on iOS now instead of 15 MB, so the other option is to just give
// up on lazy wireguard config and blow the memory and hope for the best on iOS.
// That's sad too. Or we get rid of these knobs (lazy wireguard config has been
// stable!) but I'm worried that a future regression would be easier to debug
// with these knobs in place.
func forceFullWireguardConfig(numPeers int) bool {
	// Did the user explicitly enable trimming via the environment variable knob?
	if b, ok := debugTrimWireguard().Get(); ok {
		return !b
	}
	if opt := controlclient.TrimWGConfig(); opt != "" {
		return !opt.EqualBool(true)
	}
	return false
}

// isTrimmablePeer reports whether p is a peer that we can trim out of the
// network map.
//
// For implementation simplicity, we can only trim peers that have
// only non-subnet AllowedIPs (an IPv4 /32 or IPv6 /128), which is the
// common case for most peers. Subnet router nodes will just always be
// created in the wireguard-go config.
func isTrimmablePeer(p *wgcfg.Peer, numPeers int) bool {
	if forceFullWireguardConfig(numPeers) {
		return false
	}

	// AllowedIPs must all be single IPs, not subnets.
	for _, aip := range p.AllowedIPs {
		if !aip.IsSingleIP() {
			return false
		}
	}
	return true
}

// noteRecvActivity is called by magicsock when a packet has been
// received for the peer with node key nk. Magicsock calls this no
// more than every 10 seconds for a given peer.
func (e *userspaceEngine) noteRecvActivity(nk key.NodePublic) {
	e.wgLock.Lock()
	defer e.wgLock.Unlock()

	if _, ok := e.recvActivityAt[nk]; !ok {
		// Not a trimmable peer we care about tracking. (See isTrimmablePeer)
		if e.trimmedNodes[nk] {
			e.logf("wgengine: [unexpected] noteReceiveActivity called on idle node %v that's not in recvActivityAt", nk.ShortString())
		}
		return
	}
	now := e.timeNow()
	e.recvActivityAt[nk] = now

	// As long as there's activity, periodically poll the engine to get
	// stats for the far away side effect of
	// ipn/ipnlocal.LocalBackend.parseWgStatusLocked to log activity, for
	// use in various admin dashboards.
	// This particularly matters on platforms without a connected GUI, as
	// the GUIs generally poll this enough to cause that logging. But
	// tailscaled alone did not, hence this.
	if e.lastStatusPollTime.IsZero() || now.Sub(e.lastStatusPollTime) >= statusPollInterval {
		e.lastStatusPollTime = now
		go e.RequestStatus()
	}

	// If the last activity time jumped a bunch (say, at least
	// half the idle timeout) then see if we need to reprogram
	// WireGuard. This could probably be just
	// lazyPeerIdleThreshold without the divide by 2, but
	// maybeReconfigWireguardLocked is cheap enough to call every
	// couple minutes (just not on every packet).
	if e.trimmedNodes[nk] {
		e.logf("wgengine: idle peer %v now active, reconfiguring WireGuard", nk.ShortString())
		e.maybeReconfigWireguardLocked(nil)
	}
}

// isActiveSinceLocked reports whether the peer identified by (nk, ip)
// has had a packet sent to or received from it since t.
//
// e.wgLock must be held.
func (e *userspaceEngine) isActiveSinceLocked(nk key.NodePublic, ip netip.Addr, t mono.Time) bool {
	if e.recvActivityAt[nk].After(t) {
		return true
	}
	timePtr, ok := e.sentActivityAt[ip]
	if !ok {
		return false
	}
	return timePtr.LoadAtomic().After(t)
}

// discoChanged are the set of peers whose disco keys have changed, implying they've restarted.
// If a peer is in this set and was previously in the live wireguard config,
// it needs to be first removed and then re-added to flush out its wireguard session key.
// If discoChanged is nil or empty, this extra removal step isn't done.
//
// e.wgLock must be held.
func (e *userspaceEngine) maybeReconfigWireguardLocked(discoChanged map[key.NodePublic]bool) error {
	if hook := e.testMaybeReconfigHook; hook != nil {
		hook()
		return nil
	}

	full := e.lastCfgFull
	e.wgLogger.SetPeers(full.Peers)

	// Compute a minimal config to pass to wireguard-go
	// based on the full config. Prune off all the peers
	// and only add the active ones back.
	min := full
	min.Peers = make([]wgcfg.Peer, 0, e.lastNMinPeers)

	// We'll only keep a peer around if it's been active in
	// the past 5 minutes. That's more than WireGuard's key
	// rotation time anyway so it's no harm if we remove it
	// later if it's been inactive.
	activeCutoff := e.timeNow().Add(-lazyPeerIdleThreshold)

	// Not all peers can be trimmed from the network map (see
	// isTrimmablePeer). For those that are trimmable, keep track of
	// their NodeKey and Tailscale IPs. These are the ones we'll need
	// to install tracking hooks for to watch their send/receive
	// activity.
	trackNodes := make([]key.NodePublic, 0, len(full.Peers))
	trackIPs := make([]netip.Addr, 0, len(full.Peers))

	// Don't re-alloc the map; the Go compiler optimizes map clears as of
	// Go 1.11, so we can re-use the existing + allocated map.
	if e.trimmedNodes != nil {
		maps.Clear(e.trimmedNodes)
	} else {
		e.trimmedNodes = make(map[key.NodePublic]bool)
	}

	needRemoveStep := false
	for i := range full.Peers {
		p := &full.Peers[i]
		nk := p.PublicKey
		if !isTrimmablePeer(p, len(full.Peers)) {
			min.Peers = append(min.Peers, *p)
			if discoChanged[nk] {
				needRemoveStep = true
			}
			continue
		}
		trackNodes = append(trackNodes, nk)
		recentlyActive := false
		for _, cidr := range p.AllowedIPs {
			trackIPs = append(trackIPs, cidr.Addr())
			recentlyActive = recentlyActive || e.isActiveSinceLocked(nk, cidr.Addr(), activeCutoff)
		}
		if recentlyActive {
			min.Peers = append(min.Peers, *p)
			if discoChanged[nk] {
				needRemoveStep = true
			}
		} else {
			e.trimmedNodes[nk] = true
		}
	}
	e.lastNMinPeers = len(min.Peers)

	if changed := deephash.Update(&e.lastEngineSigTrim, &struct {
		WGConfig     *wgcfg.Config
		TrimmedNodes map[key.NodePublic]bool
		TrackNodes   []key.NodePublic
		TrackIPs     []netip.Addr
	}{&min, e.trimmedNodes, trackNodes, trackIPs}); !changed {
		return nil
	}

	e.updateActivityMapsLocked(trackNodes, trackIPs)

	if needRemoveStep {
		minner := min
		minner.Peers = nil
		numRemove := 0
		for _, p := range min.Peers {
			if discoChanged[p.PublicKey] {
				numRemove++
				continue
			}
			minner.Peers = append(minner.Peers, p)
		}
		if numRemove > 0 {
			e.logf("wgengine: Reconfig: removing session keys for %d peers", numRemove)
			if err := wgcfg.ReconfigDevice(e.wgdev, &minner, e.logf); err != nil {
				e.logf("wgdev.Reconfig: %v", err)
				return err
			}
		}
	}

	e.logf("wgengine: Reconfig: configuring userspace WireGuard config (with %d/%d peers)", len(min.Peers), len(full.Peers))
	if err := wgcfg.ReconfigDevice(e.wgdev, &min, e.logf); err != nil {
		e.logf("wgdev.Reconfig: %v", err)
		return err
	}
	return nil
}

// updateActivityMapsLocked updates the data structures used for tracking the activity
// of wireguard peers that we might add/remove dynamically from the real config
// as given to wireguard-go.
//
// e.wgLock must be held.
func (e *userspaceEngine) updateActivityMapsLocked(trackNodes []key.NodePublic, trackIPs []netip.Addr) {
	// Generate the new map of which nodekeys we want to track
	// receive times for.
	mr := map[key.NodePublic]mono.Time{} // TODO: only recreate this if set of keys changed
	for _, nk := range trackNodes {
		// Preserve old times in the new map, but also
		// populate map entries for new trackNodes values with
		// time.Time{} zero values. (Only entries in this map
		// are tracked, so the Time zero values allow it to be
		// tracked later)
		mr[nk] = e.recvActivityAt[nk]
	}
	e.recvActivityAt = mr

	oldTime := e.sentActivityAt
	e.sentActivityAt = make(map[netip.Addr]*mono.Time, len(oldTime))
	oldFunc := e.destIPActivityFuncs
	e.destIPActivityFuncs = make(map[netip.Addr]func(), len(oldFunc))

	updateFn := func(timePtr *mono.Time) func() {
		return func() {
			now := e.timeNow()
			old := timePtr.LoadAtomic()

			// How long's it been since we last sent a packet?
			elapsed := now.Sub(old)
			if old == 0 {
				// For our first packet, old is 0, which has indeterminate meaning.
				// Set elapsed to a big number (four score and seven years).
				elapsed = 762642 * time.Hour
			}

			if elapsed >= packetSendTimeUpdateFrequency {
				timePtr.StoreAtomic(now)
			}
			// On a big jump, assume we might no longer be in the wireguard
			// config and go check.
			if elapsed >= packetSendRecheckWireguardThreshold {
				e.wgLock.Lock()
				defer e.wgLock.Unlock()
				e.maybeReconfigWireguardLocked(nil)
			}
		}
	}

	for _, ip := range trackIPs {
		timePtr := oldTime[ip]
		if timePtr == nil {
			timePtr = new(mono.Time)
		}
		e.sentActivityAt[ip] = timePtr

		fn := oldFunc[ip]
		if fn == nil {
			fn = updateFn(timePtr)
		}
		e.destIPActivityFuncs[ip] = fn
	}
	e.tundev.SetDestIPActivityFuncs(e.destIPActivityFuncs)
}

// hasOverlap checks if there is a IPPrefix which is common amongst the two
// provided slices.
func hasOverlap(aips, rips []netip.Prefix) bool {
	for _, aip := range aips {
		for _, rip := range rips {
			if aip == rip {
				return true
			}
		}
	}
	return false
}

func (e *userspaceEngine) Reconfig(cfg *wgcfg.Config, routerCfg *router.Config, dnsCfg *dns.Config, debug *tailcfg.Debug) error {
	if routerCfg == nil {
		panic("routerCfg must not be nil")
	}
	if dnsCfg == nil {
		panic("dnsCfg must not be nil")
	}

	e.isLocalAddr.Store(tsaddr.NewContainsIPFunc(routerCfg.LocalAddrs))

	e.wgLock.Lock()
	defer e.wgLock.Unlock()
	e.lastDNSConfig = dnsCfg

	peerSet := make(map[key.NodePublic]struct{}, len(cfg.Peers))
	e.mu.Lock()
	e.peerSequence = e.peerSequence[:0]
	for _, p := range cfg.Peers {
		e.peerSequence = append(e.peerSequence, p.PublicKey)
		peerSet[p.PublicKey] = struct{}{}
	}
	nm := e.netMap
	e.mu.Unlock()

	listenPort := e.confListenPort
	if debug != nil && debug.RandomizeClientPort {
		listenPort = 0
	}

	isSubnetRouter := false
	if e.birdClient != nil && nm != nil && nm.SelfNode != nil {
		isSubnetRouter = hasOverlap(nm.SelfNode.PrimaryRoutes, nm.Hostinfo.RoutableIPs)
		e.logf("[v1] Reconfig: hasOverlap(%v, %v) = %v; isSubnetRouter=%v lastIsSubnetRouter=%v",
			nm.SelfNode.PrimaryRoutes, nm.Hostinfo.RoutableIPs,
			isSubnetRouter, isSubnetRouter, e.lastIsSubnetRouter)
	}
	isSubnetRouterChanged := isSubnetRouter != e.lastIsSubnetRouter

	engineChanged := deephash.Update(&e.lastEngineSigFull, cfg)
	routerChanged := deephash.Update(&e.lastRouterSig, &struct {
		RouterConfig *router.Config
		DNSConfig    *dns.Config
	}{routerCfg, dnsCfg})
	if !engineChanged && !routerChanged && listenPort == e.magicConn.LocalPort() && !isSubnetRouterChanged {
		return ErrNoChanges
	}
	newLogIDs := cfg.NetworkLogging
	oldLogIDs := e.lastCfgFull.NetworkLogging
	netLogIDsNowValid := !newLogIDs.NodeID.IsZero() && !newLogIDs.DomainID.IsZero()
	netLogIDsWasValid := !oldLogIDs.NodeID.IsZero() && !oldLogIDs.DomainID.IsZero()
	netLogIDsChanged := netLogIDsNowValid && netLogIDsWasValid && newLogIDs != oldLogIDs
	netLogRunning := netLogIDsNowValid && !routerCfg.Equal(&router.Config{})
	if envknob.NoLogsNoSupport() {
		netLogRunning = false
	}

	// TODO(bradfitz,danderson): maybe delete this isDNSIPOverTailscale
	// field and delete the resolver.ForwardLinkSelector hook and
	// instead have ipnlocal populate a map of DNS IP => linkName and
	// put that in the *dns.Config instead, and plumb it down to the
	// dns.Manager. Maybe also with isLocalAddr above.
	e.isDNSIPOverTailscale.Store(tsaddr.NewContainsIPFunc(dnsIPsOverTailscale(dnsCfg, routerCfg)))

	// See if any peers have changed disco keys, which means they've restarted.
	// If so, we need to update the wireguard-go/device.Device in two phases:
	// once without the node which has restarted, to clear its wireguard session key,
	// and a second time with it.
	discoChanged := make(map[key.NodePublic]bool)
	{
		prevEP := make(map[key.NodePublic]key.DiscoPublic)
		for i := range e.lastCfgFull.Peers {
			if p := &e.lastCfgFull.Peers[i]; !p.DiscoKey.IsZero() {
				prevEP[p.PublicKey] = p.DiscoKey
			}
		}
		for i := range cfg.Peers {
			p := &cfg.Peers[i]
			if p.DiscoKey.IsZero() {
				continue
			}
			pub := p.PublicKey
			if old, ok := prevEP[pub]; ok && old != p.DiscoKey {
				discoChanged[pub] = true
				e.logf("wgengine: Reconfig: %s changed from %q to %q", pub.ShortString(), old, p.DiscoKey)
			}
		}
	}

	e.lastCfgFull = *cfg.Clone()

	// Tell magicsock about the new (or initial) private key
	// (which is needed by DERP) before wgdev gets it, as wgdev
	// will start trying to handshake, which we want to be able to
	// go over DERP.
	if err := e.magicConn.SetPrivateKey(cfg.PrivateKey); err != nil {
		e.logf("wgengine: Reconfig: SetPrivateKey: %v", err)
	}
	e.magicConn.UpdatePeers(peerSet)
	e.magicConn.SetPreferredPort(listenPort)

	if err := e.maybeReconfigWireguardLocked(discoChanged); err != nil {
		return err
	}

	// Shutdown the network logger because the IDs changed.
	// Let it be started back up by subsequent logic.
	if netLogIDsChanged && e.networkLogger.Running() {
		e.logf("wgengine: Reconfig: shutting down network logger")
		ctx, cancel := context.WithTimeout(context.Background(), networkLoggerUploadTimeout)
		defer cancel()
		if err := e.networkLogger.Shutdown(ctx); err != nil {
			e.logf("wgengine: Reconfig: error shutting down network logger: %v", err)
		}
	}

	// Startup the network logger.
	// Do this before configuring the router so that we capture initial packets.
	if netLogRunning && !e.networkLogger.Running() {
		nid := cfg.NetworkLogging.NodeID
		tid := cfg.NetworkLogging.DomainID
		e.logf("wgengine: Reconfig: starting up network logger (node:%s tailnet:%s)", nid.Public(), tid.Public())
		if err := e.networkLogger.Startup(cfg.NodeID, nid, tid, e.tundev, e.magicConn); err != nil {
			e.logf("wgengine: Reconfig: error starting up network logger: %v", err)
		}
		e.networkLogger.ReconfigRoutes(routerCfg)
	}

	if routerChanged {
		e.logf("wgengine: Reconfig: configuring router")
		e.networkLogger.ReconfigRoutes(routerCfg)
		err := e.router.Set(routerCfg)
		health.SetRouterHealth(err)
		if err != nil {
			return err
		}
		// Keep DNS configuration after router configuration, as some
		// DNS managers refuse to apply settings if the device has no
		// assigned address.
		e.logf("wgengine: Reconfig: configuring DNS")
		err = e.dns.Set(*dnsCfg)
		health.SetDNSHealth(err)
		if err != nil {
			return err
		}
	}

	// Shutdown the network logger.
	// Do this after configuring the router so that we capture final packets.
	// This attempts to flush out any log messages and may block.
	if !netLogRunning && e.networkLogger.Running() {
		e.logf("wgengine: Reconfig: shutting down network logger")
		ctx, cancel := context.WithTimeout(context.Background(), networkLoggerUploadTimeout)
		defer cancel()
		if err := e.networkLogger.Shutdown(ctx); err != nil {
			e.logf("wgengine: Reconfig: error shutting down network logger: %v", err)
		}
	}

	if isSubnetRouterChanged && e.birdClient != nil {
		e.logf("wgengine: Reconfig: configuring BIRD")
		var err error
		if isSubnetRouter {
			err = e.birdClient.EnableProtocol("tailscale")
		} else {
			err = e.birdClient.DisableProtocol("tailscale")
		}
		if err != nil {
			// Log but don't fail here.
			e.logf("wgengine: error configuring BIRD: %v", err)
		} else {
			e.lastIsSubnetRouter = isSubnetRouter
		}
	}

	e.logf("[v1] wgengine: Reconfig done")
	return nil
}

func (e *userspaceEngine) GetFilter() *filter.Filter {
	return e.tundev.GetFilter()
}

func (e *userspaceEngine) SetFilter(filt *filter.Filter) {
	e.tundev.SetFilter(filt)
}

func (e *userspaceEngine) SetStatusCallback(cb StatusCallback) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statusCallback = cb
}

func (e *userspaceEngine) getStatusCallback() StatusCallback {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statusCallback
}

var singleNewline = []byte{'\n'}

var ErrEngineClosing = errors.New("engine closing; no status")

func (e *userspaceEngine) getPeerStatusLite(pk key.NodePublic) (status ipnstate.PeerStatusLite, ok bool) {
	e.wgLock.Lock()
	if e.wgdev == nil {
		e.wgLock.Unlock()
		return status, false
	}
	peer := e.wgdev.LookupPeer(pk.Raw32())
	e.wgLock.Unlock()
	if peer == nil {
		return status, false
	}
	status.NodeKey = pk
	status.RxBytes = int64(wgint.PeerRxBytes(peer))
	status.TxBytes = int64(wgint.PeerTxBytes(peer))
	status.LastHandshake = time.Unix(0, wgint.PeerLastHandshakeNano(peer))
	return status, true
}

func (e *userspaceEngine) getStatus() (*Status, error) {
	// Grab derpConns before acquiring wgLock to not violate lock ordering;
	// the DERPs method acquires magicsock.Conn.mu.
	// (See comment in userspaceEngine's declaration.)
	derpConns := e.magicConn.DERPs()

	e.mu.Lock()
	closing := e.closing
	peerKeys := make([]key.NodePublic, len(e.peerSequence))
	copy(peerKeys, e.peerSequence)
	localAddrs := append([]tailcfg.Endpoint(nil), e.endpoints...)
	e.mu.Unlock()

	if closing {
		return nil, ErrEngineClosing
	}

	peers := make([]ipnstate.PeerStatusLite, 0, len(peerKeys))
	for _, key := range peerKeys {
		if status, found := e.getPeerStatusLite(key); found {
			peers = append(peers, status)
		}
	}

	return &Status{
		AsOf:       time.Now(),
		LocalAddrs: localAddrs,
		Peers:      peers,
		DERPs:      derpConns,
	}, nil
}

func (e *userspaceEngine) RequestStatus() {
	// This is slightly tricky. e.getStatus() can theoretically get
	// blocked inside wireguard for a while, and RequestStatus() is
	// sometimes called from a goroutine, so we don't want a lot of
	// them hanging around. On the other hand, requesting multiple
	// status updates simultaneously is pointless anyway; they will
	// all say the same thing.

	// Enqueue at most one request. If one is in progress already, this
	// adds one more to the queue. If one has been requested but not
	// started, it is a no-op.
	select {
	case e.reqCh <- struct{}{}:
	default:
	}

	// Dequeue at most one request. Another thread may have already
	// dequeued the request we enqueued above, which is fine, since the
	// information is guaranteed to be at least as recent as the current
	// call to RequestStatus().
	select {
	case <-e.reqCh:
		s, err := e.getStatus()
		if s == nil && err == nil {
			e.logf("[unexpected] RequestStatus: both s and err are nil")
			return
		}
		if cb := e.getStatusCallback(); cb != nil {
			cb(s, err)
		}
	default:
	}
}

func (e *userspaceEngine) Close() {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return
	}
	e.closing = true
	e.mu.Unlock()

	r := bufio.NewReader(strings.NewReader(""))
	e.wgdev.IpcSetOperation(r)
	e.magicConn.Close()
	e.linkMonUnregister()
	if e.linkMonOwned {
		e.linkMon.Close()
	}
	e.dns.Down()
	e.router.Close()
	e.wgdev.Close()
	e.tundev.Close()
	if e.birdClient != nil {
		e.birdClient.DisableProtocol("tailscale")
		e.birdClient.Close()
	}
	close(e.waitCh)

	ctx, cancel := context.WithTimeout(context.Background(), networkLoggerUploadTimeout)
	defer cancel()
	if err := e.networkLogger.Shutdown(ctx); err != nil {
		e.logf("wgengine: Close: error shutting down network logger: %v", err)
	}
}

func (e *userspaceEngine) Wait() {
	<-e.waitCh
}

func (e *userspaceEngine) GetLinkMonitor() *monitor.Mon {
	return e.linkMon
}

// LinkChange signals a network change event. It's currently
// (2021-03-03) only called on Android. On other platforms, linkMon
// generates link change events for us.
func (e *userspaceEngine) LinkChange(_ bool) {
	e.linkMon.InjectEvent()
}

func (e *userspaceEngine) linkChange(changed bool, cur *interfaces.State) {
	up := cur.AnyInterfaceUp()
	if !up {
		e.logf("LinkChange: all links down; pausing: %v", cur)
	} else if changed {
		e.logf("LinkChange: major, rebinding. New state: %v", cur)
	} else {
		e.logf("[v1] LinkChange: minor")
	}

	health.SetAnyInterfaceUp(up)
	e.magicConn.SetNetworkUp(up)
	if !up || changed {
		if err := e.dns.FlushCaches(); err != nil {
			e.logf("wgengine: dns flush failed after major link change: %v", err)
		}
	}

	// Hacky workaround for Linux DNS issue 2458: on
	// suspend/resume or whenever NetworkManager is started, it
	// nukes all systemd-resolved configs. So reapply our DNS
	// config on major link change.
	if changed {
		switch runtime.GOOS {
		case "linux", "android", "ios", "darwin":
			e.wgLock.Lock()
			dnsCfg := e.lastDNSConfig
			e.wgLock.Unlock()
			if dnsCfg != nil {
				if err := e.dns.Set(*dnsCfg); err != nil {
					e.logf("wgengine: error setting DNS config after major link change: %v", err)
				} else {
					e.logf("wgengine: set DNS config again after major link change")
				}
			}
		}
	}

	why := "link-change-minor"
	if changed {
		why = "link-change-major"
		metricNumMajorChanges.Add(1)
		e.magicConn.Rebind()
	} else {
		metricNumMinorChanges.Add(1)
	}
	e.magicConn.ReSTUN(why)
}

func (e *userspaceEngine) AddNetworkMapCallback(cb NetworkMapCallback) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.networkMapCallbacks == nil {
		e.networkMapCallbacks = make(map[*someHandle]NetworkMapCallback)
	}
	h := new(someHandle)
	e.networkMapCallbacks[h] = cb
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.networkMapCallbacks, h)
	}
}

func (e *userspaceEngine) SetNetInfoCallback(cb NetInfoCallback) {
	e.magicConn.SetNetInfoCallback(cb)
}

func (e *userspaceEngine) SetDERPMap(dm *tailcfg.DERPMap) {
	e.magicConn.SetDERPMap(dm)
}

func (e *userspaceEngine) SetNetworkMap(nm *netmap.NetworkMap) {
	e.magicConn.SetNetworkMap(nm)
	e.mu.Lock()
	e.netMap = nm
	callbacks := make([]NetworkMapCallback, 0, 4)
	for _, fn := range e.networkMapCallbacks {
		callbacks = append(callbacks, fn)
	}
	e.mu.Unlock()
	for _, fn := range callbacks {
		fn(nm)
	}
}

func (e *userspaceEngine) DiscoPublicKey() key.DiscoPublic {
	return e.magicConn.DiscoPublicKey()
}

func (e *userspaceEngine) UpdateStatus(sb *ipnstate.StatusBuilder) {
	st, err := e.getStatus()
	if err != nil {
		e.logf("wgengine: getStatus: %v", err)
		return
	}
	if sb.WantPeers {
		for _, ps := range st.Peers {
			sb.AddPeer(ps.NodeKey, &ipnstate.PeerStatus{
				RxBytes:       int64(ps.RxBytes),
				TxBytes:       int64(ps.TxBytes),
				LastHandshake: ps.LastHandshake,
				InEngine:      true,
			})
		}
	}

	e.magicConn.UpdateStatus(sb)
}

func (e *userspaceEngine) Ping(ip netip.Addr, pingType tailcfg.PingType, cb func(*ipnstate.PingResult)) {
	res := &ipnstate.PingResult{IP: ip.String()}
	pip, ok := e.PeerForIP(ip)
	if !ok {
		e.logf("ping(%v): no matching peer", ip)
		res.Err = "no matching peer"
		cb(res)
		return
	}
	if pip.IsSelf {
		res.Err = fmt.Sprintf("%v is local Tailscale IP", ip)
		res.IsLocalIP = true
		cb(res)
		return
	}
	peer := pip.Node

	e.logf("ping(%v): sending %v ping to %v %v ...", ip, pingType, peer.Key.ShortString(), peer.ComputedName)
	switch pingType {
	case "disco":
		e.magicConn.Ping(peer, res, cb)
	case "TSMP":
		e.sendTSMPPing(ip, peer, res, cb)
	case "ICMP":
		e.sendICMPEchoRequest(ip, peer, res, cb)
	}
}

func (e *userspaceEngine) mySelfIPMatchingFamily(dst netip.Addr) (src netip.Addr, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.netMap == nil {
		return netip.Addr{}, errors.New("no netmap")
	}
	for _, a := range e.netMap.Addresses {
		if a.IsSingleIP() && a.Addr().BitLen() == dst.BitLen() {
			return a.Addr(), nil
		}
	}
	if len(e.netMap.Addresses) == 0 {
		return netip.Addr{}, errors.New("no self address in netmap")
	}
	return netip.Addr{}, errors.New("no self address in netmap matching address family")
}

func (e *userspaceEngine) sendICMPEchoRequest(destIP netip.Addr, peer *tailcfg.Node, res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	srcIP, err := e.mySelfIPMatchingFamily(destIP)
	if err != nil {
		res.Err = err.Error()
		cb(res)
		return
	}
	var icmph packet.Header
	if srcIP.Is4() {
		icmph = packet.ICMP4Header{
			IP4Header: packet.IP4Header{
				IPProto: ipproto.ICMPv4,
				Src:     srcIP,
				Dst:     destIP,
			},
			Type: packet.ICMP4EchoRequest,
			Code: packet.ICMP4NoCode,
		}
	} else {
		icmph = packet.ICMP6Header{
			IP6Header: packet.IP6Header{
				IPProto: ipproto.ICMPv6,
				Src:     srcIP,
				Dst:     destIP,
			},
			Type: packet.ICMP6EchoRequest,
			Code: packet.ICMP6NoCode,
		}
	}

	idSeq, payload := packet.ICMPEchoPayload(nil)

	expireTimer := time.AfterFunc(10*time.Second, func() {
		e.setICMPEchoResponseCallback(idSeq, nil)
	})
	t0 := time.Now()
	e.setICMPEchoResponseCallback(idSeq, func() {
		expireTimer.Stop()
		d := time.Since(t0)
		res.LatencySeconds = d.Seconds()
		res.NodeIP = destIP.String()
		res.NodeName = peer.ComputedName
		cb(res)
	})

	icmpPing := packet.Generate(icmph, payload)
	e.tundev.InjectOutbound(icmpPing)
}

func (e *userspaceEngine) sendTSMPPing(ip netip.Addr, peer *tailcfg.Node, res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	srcIP, err := e.mySelfIPMatchingFamily(ip)
	if err != nil {
		res.Err = err.Error()
		cb(res)
		return
	}
	var iph packet.Header
	if srcIP.Is4() {
		iph = packet.IP4Header{
			IPProto: ipproto.TSMP,
			Src:     srcIP,
			Dst:     ip,
		}
	} else {
		iph = packet.IP6Header{
			IPProto: ipproto.TSMP,
			Src:     srcIP,
			Dst:     ip,
		}
	}

	var data [8]byte
	crand.Read(data[:])

	expireTimer := time.AfterFunc(10*time.Second, func() {
		e.setTSMPPongCallback(data, nil)
	})
	t0 := time.Now()
	e.setTSMPPongCallback(data, func(pong packet.TSMPPongReply) {
		expireTimer.Stop()
		d := time.Since(t0)
		res.LatencySeconds = d.Seconds()
		res.NodeIP = ip.String()
		res.NodeName = peer.ComputedName
		res.PeerAPIPort = pong.PeerAPIPort
		cb(res)
	})

	var tsmpPayload [9]byte
	tsmpPayload[0] = byte(packet.TSMPTypePing)
	copy(tsmpPayload[1:], data[:])

	tsmpPing := packet.Generate(iph, tsmpPayload[:])
	e.tundev.InjectOutbound(tsmpPing)
}

func (e *userspaceEngine) setTSMPPongCallback(data [8]byte, cb func(packet.TSMPPongReply)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pongCallback == nil {
		e.pongCallback = map[[8]byte]func(packet.TSMPPongReply){}
	}
	if cb == nil {
		delete(e.pongCallback, data)
	} else {
		e.pongCallback[data] = cb
	}
}

func (e *userspaceEngine) setICMPEchoResponseCallback(idSeq uint32, cb func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cb == nil {
		delete(e.icmpEchoResponseCallback, idSeq)
	} else {
		mak.Set(&e.icmpEchoResponseCallback, idSeq, cb)
	}
}

func (e *userspaceEngine) RegisterIPPortIdentity(ipport netip.AddrPort, tsIP netip.Addr) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tsIPByIPPort == nil {
		e.tsIPByIPPort = make(map[netip.AddrPort]netip.Addr)
	}
	e.tsIPByIPPort[ipport] = tsIP
}

func (e *userspaceEngine) UnregisterIPPortIdentity(ipport netip.AddrPort) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tsIPByIPPort == nil {
		return
	}
	delete(e.tsIPByIPPort, ipport)
}

var whoIsSleeps = [...]time.Duration{
	0,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
}

func (e *userspaceEngine) WhoIsIPPort(ipport netip.AddrPort) (tsIP netip.Addr, ok bool) {
	// We currently have a registration race,
	// https://github.com/tailscale/tailscale/issues/1616,
	// so loop a few times for now waiting for the registration
	// to appear.
	// TODO(bradfitz,namansood): remove this once #1616 is fixed.
	for _, d := range whoIsSleeps {
		time.Sleep(d)
		e.mu.Lock()
		tsIP, ok = e.tsIPByIPPort[ipport]
		e.mu.Unlock()
		if ok {
			return tsIP, true
		}
	}
	return tsIP, false
}

// PeerForIP returns the Node in the wireguard config
// that's responsible for handling the given IP address.
//
// If none is found in the wireguard config but one is found in
// the netmap, it's described in an error.
//
// peerForIP acquires both e.mu and e.wgLock, but neither at the same
// time.
func (e *userspaceEngine) PeerForIP(ip netip.Addr) (ret PeerForIP, ok bool) {
	e.mu.Lock()
	nm := e.netMap
	e.mu.Unlock()
	if nm == nil {
		return ret, false
	}

	// Check for exact matches before looking for subnet matches.
	// TODO(bradfitz): add maps for these. on NetworkMap?
	for _, p := range nm.Peers {
		for _, a := range p.Addresses {
			if a.Addr() == ip && a.IsSingleIP() && tsaddr.IsTailscaleIP(ip) {
				return PeerForIP{Node: p, Route: a}, true
			}
		}
	}
	for _, a := range nm.Addresses {
		if a.Addr() == ip && a.IsSingleIP() && tsaddr.IsTailscaleIP(ip) {
			return PeerForIP{Node: nm.SelfNode, IsSelf: true, Route: a}, true
		}
	}

	e.wgLock.Lock()
	defer e.wgLock.Unlock()

	// TODO(bradfitz): this is O(n peers). Add ART to netaddr?
	var best netip.Prefix
	var bestKey key.NodePublic
	for _, p := range e.lastCfgFull.Peers {
		for _, cidr := range p.AllowedIPs {
			if !cidr.Contains(ip) {
				continue
			}
			if !best.IsValid() || cidr.Bits() > best.Bits() {
				best = cidr
				bestKey = p.PublicKey
			}
		}
	}
	// And another pass. Probably better than allocating a map per peerForIP
	// call. But TODO(bradfitz): add a lookup map to netmap.NetworkMap.
	if !bestKey.IsZero() {
		for _, p := range nm.Peers {
			if p.Key == bestKey {
				return PeerForIP{Node: p, Route: best}, true
			}
		}
	}
	return ret, false
}

type closeOnErrorPool []func()

func (p *closeOnErrorPool) add(c io.Closer)   { *p = append(*p, func() { c.Close() }) }
func (p *closeOnErrorPool) addFunc(fn func()) { *p = append(*p, fn) }
func (p closeOnErrorPool) closeAllIfError(errp *error) {
	if *errp != nil {
		for _, closeFn := range p {
			closeFn()
		}
	}
}

// ipInPrefixes reports whether ip is in any of pp.
func ipInPrefixes(ip netip.Addr, pp []netip.Prefix) bool {
	for _, p := range pp {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// dnsIPsOverTailscale returns the IPPrefixes of DNS resolver IPs that are
// routed over Tailscale. The returned value does not contain duplicates is
// not necessarily sorted.
func dnsIPsOverTailscale(dnsCfg *dns.Config, routerCfg *router.Config) (ret []netip.Prefix) {
	m := map[netip.Addr]bool{}

	add := func(resolvers []*dnstype.Resolver) {
		for _, r := range resolvers {
			ip, err := netip.ParseAddr(r.Addr)
			if err != nil {
				if ipp, err := netip.ParseAddrPort(r.Addr); err == nil {
					ip = ipp.Addr()
				} else {
					continue
				}
			}
			if ipInPrefixes(ip, routerCfg.Routes) && !ipInPrefixes(ip, routerCfg.LocalRoutes) {
				m[ip] = true
			}
		}
	}

	add(dnsCfg.DefaultResolvers)
	for _, resolvers := range dnsCfg.Routes {
		add(resolvers)
	}

	ret = make([]netip.Prefix, 0, len(m))
	for ip := range m {
		ret = append(ret, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return ret
}

// fwdDNSLinkSelector is userspaceEngine's resolver.ForwardLinkSelector, to pick
// which network interface to send DNS queries out of.
type fwdDNSLinkSelector struct {
	ue      *userspaceEngine
	tunName string
}

func (ls fwdDNSLinkSelector) PickLink(ip netip.Addr) (linkName string) {
	if ls.ue.isDNSIPOverTailscale.Load()(ip) {
		return ls.tunName
	}
	return ""
}

var (
	metricMagicDNSPacketIn = clientmetric.NewCounter("magicdns_packet_in") // for 100.100.100.100
	metricReflectToOS      = clientmetric.NewCounter("packet_reflect_to_os")

	metricNumMajorChanges = clientmetric.NewCounter("wgengine_major_changes")
	metricNumMinorChanges = clientmetric.NewCounter("wgengine_minor_changes")
)

func (e *userspaceEngine) InstallCaptureHook(cb capture.Callback) {
	e.tundev.InstallCaptureHook(cb)
	e.magicConn.InstallCaptureHook(cb)
}
