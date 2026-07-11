package routefluentgroup

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	boxdns "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"

	mDNS "github.com/miekg/dns"
)

const (
	modeLocalOnly = "local_only"

	defaultFailureThreshold         = 2
	defaultRecoverySuccessThreshold = 2
	defaultProbeInterval            = 30 * time.Second
	defaultUnhealthyCooldown        = 20 * time.Second
	defaultFallbackTTLCap           = 60 * time.Second
)

const (
	stateHealthy = iota
	stateSuspect
	stateUnhealthy
	stateProbing
)

var _ adapter.DNSTransport = (*Transport)(nil)
var _ adapter.DNSCacheControlTransport = (*Transport)(nil)

func RegisterTransport(registry *boxdns.TransportRegistry) {
	boxdns.RegisterTransport[option.RouteFluentResolverGroupDNSServerOptions](registry, C.DNSTypeRouteFluentResolverGroup, NewTransport)
}

type primaryState struct {
	tag       string
	transport adapter.DNSTransport
	state     int
	failures  int
	successes int
	nextProbe time.Time
	lastError string
}

type Transport struct {
	boxdns.TransportAdapter

	logger                   log.ContextLogger
	manager                  adapter.DNSTransportManager
	primaryTags              []string
	fallbackTag              string
	fallbackEnabled          bool
	localOnly                bool
	probeDomains             []string
	failureThreshold         int
	recoverySuccessThreshold int
	probeInterval            time.Duration
	unhealthyCooldown        time.Duration
	fallbackTTLCap           time.Duration

	access    sync.Mutex
	primaries []*primaryState
	fallback  adapter.DNSTransport
	closed    chan struct{}
}

func NewTransport(ctx context.Context, logger log.ContextLogger, tag string, options option.RouteFluentResolverGroupDNSServerOptions) (adapter.DNSTransport, error) {
	if tag == "" {
		return nil, E.New("routefluent resolver group tag is required")
	}
	localOnly := false
	switch options.Mode {
	case "":
	case modeLocalOnly:
		localOnly = true
	default:
		return nil, E.New("unsupported routefluent resolver group mode: ", options.Mode)
	}
	if options.FailureThreshold < 0 {
		return nil, E.New("failure_threshold must be positive")
	}
	if options.RecoverySuccessThreshold < 0 {
		return nil, E.New("recovery_success_threshold must be positive")
	}
	failureThreshold := options.FailureThreshold
	if failureThreshold == 0 {
		failureThreshold = defaultFailureThreshold
	}
	recoverySuccessThreshold := options.RecoverySuccessThreshold
	if recoverySuccessThreshold == 0 {
		recoverySuccessThreshold = defaultRecoverySuccessThreshold
	}
	probeInterval := time.Duration(options.ProbeInterval)
	if probeInterval == 0 {
		probeInterval = defaultProbeInterval
	}
	unhealthyCooldown := time.Duration(options.UnhealthyCooldown)
	if unhealthyCooldown == 0 {
		unhealthyCooldown = defaultUnhealthyCooldown
	}
	fallbackTTLCap := time.Duration(options.FallbackTTLCap)
	if fallbackTTLCap == 0 {
		fallbackTTLCap = defaultFallbackTTLCap
	}
	if probeInterval < 0 {
		return nil, E.New("probe_interval must be positive")
	}
	if unhealthyCooldown < 0 {
		return nil, E.New("unhealthy_cooldown must be positive")
	}
	if fallbackTTLCap < 0 {
		return nil, E.New("fallback_ttl_cap must be positive")
	}
	if localOnly {
		if len(options.Primary) > 0 {
			return nil, E.New("local_only routefluent resolver group must not configure primary")
		}
		if options.Fallback == "" {
			return nil, E.New("local_only routefluent resolver group requires fallback")
		}
		if !options.FallbackEnabled {
			return nil, E.New("local_only routefluent resolver group requires fallback_enabled=true")
		}
	} else {
		if len(options.Primary) == 0 {
			return nil, E.New("routefluent resolver group requires at least one primary DoH server")
		}
		if options.FallbackEnabled {
			if options.Fallback == "" {
				return nil, E.New("routefluent resolver group fallback_enabled=true requires fallback")
			}
			if len(options.ProbeDomains) == 0 {
				return nil, E.New("routefluent resolver group fallback_enabled=true requires probe_domains")
			}
		} else if options.Fallback != "" {
			return nil, E.New("routefluent resolver group fallback requires fallback_enabled=true")
		}
	}
	seen := make(map[string]bool)
	dependencies := make([]string, 0, len(options.Primary)+1)
	for _, primaryTag := range options.Primary {
		if primaryTag == "" {
			return nil, E.New("routefluent resolver group primary tag must not be empty")
		}
		if primaryTag == tag {
			return nil, E.New("routefluent resolver group primary must not reference itself")
		}
		if seen[primaryTag] {
			return nil, E.New("duplicate routefluent resolver group primary: ", primaryTag)
		}
		seen[primaryTag] = true
		dependencies = append(dependencies, primaryTag)
	}
	if options.Fallback != "" {
		if options.Fallback == tag {
			return nil, E.New("routefluent resolver group fallback must not reference itself")
		}
		if seen[options.Fallback] {
			return nil, E.New("routefluent resolver group fallback must not duplicate primary: ", options.Fallback)
		}
		dependencies = append(dependencies, options.Fallback)
	}
	manager := service.FromContext[adapter.DNSTransportManager](ctx)
	if manager == nil {
		return nil, E.New("missing DNS transport manager in context")
	}
	return &Transport{
		TransportAdapter:         boxdns.NewTransportAdapter(C.DNSTypeRouteFluentResolverGroup, tag, dependencies),
		logger:                   logger,
		manager:                  manager,
		primaryTags:              append([]string(nil), options.Primary...),
		fallbackTag:              options.Fallback,
		fallbackEnabled:          options.FallbackEnabled,
		localOnly:                localOnly,
		probeDomains:             append([]string(nil), options.ProbeDomains...),
		failureThreshold:         failureThreshold,
		recoverySuccessThreshold: recoverySuccessThreshold,
		probeInterval:            probeInterval,
		unhealthyCooldown:        unhealthyCooldown,
		fallbackTTLCap:           fallbackTTLCap,
		closed:                   make(chan struct{}),
	}, nil
}

func (t *Transport) DisableDNSCache() bool {
	return !t.localOnly
}

func (t *Transport) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if err := t.resolveTransports(); err != nil {
		return err
	}
	if !t.localOnly && t.fallbackEnabled {
		go t.probeLoop()
	}
	return nil
}

func (t *Transport) Close() error {
	select {
	case <-t.closed:
	default:
		close(t.closed)
	}
	return nil
}

func (t *Transport) Reset() {
	t.access.Lock()
	primaries := append([]*primaryState(nil), t.primaries...)
	fallback := t.fallback
	t.access.Unlock()
	for _, primary := range primaries {
		primary.transport.Reset()
	}
	if fallback != nil {
		fallback.Reset()
	}
}

func (t *Transport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	if t.localOnly {
		return t.exchangeFallback(ctx, message, "local_only")
	}
	var failures []string
	for _, primary := range t.candidatePrimaries() {
		response, err := primary.transport.Exchange(ctx, message.Copy())
		if err != nil {
			t.markFailure(primary.tag, err)
			failures = append(failures, primary.tag+": "+err.Error())
			continue
		}
		if response != nil && response.Rcode == mDNS.RcodeServerFailure {
			err = E.New("server failure")
			t.markFailure(primary.tag, err)
			failures = append(failures, primary.tag+": SERVFAIL")
			continue
		}
		t.markHealthy(primary.tag, true)
		t.logger.Debug("routefluent resolver group[", t.Tag(), "] selected primary resolver[", primary.tag, "]")
		return response, nil
	}
	if !t.fallbackEnabled {
		if len(failures) == 0 {
			return nil, E.New("routefluent resolver group[", t.Tag(), "] has no available primary DoH resolver")
		}
		return nil, E.New("routefluent resolver group[", t.Tag(), "] primary DoH resolvers unavailable: ", strings.Join(failures, "; "))
	}
	reason := "all_primary_unavailable"
	if len(failures) > 0 {
		reason += ": " + strings.Join(failures, "; ")
	}
	return t.exchangeFallback(ctx, message, reason)
}

func (t *Transport) resolveTransports() error {
	t.access.Lock()
	defer t.access.Unlock()
	if t.localOnly {
		fallback, err := t.lookupFallback()
		if err != nil {
			return err
		}
		t.fallback = fallback
		return nil
	}
	primaries := make([]*primaryState, 0, len(t.primaryTags))
	for _, primaryTag := range t.primaryTags {
		transport, loaded := t.manager.Transport(primaryTag)
		if !loaded {
			return E.New("routefluent resolver group[", t.Tag(), "] primary resolver not found: ", primaryTag)
		}
		switch transport.Type() {
		case C.DNSTypeHTTPS, C.DNSTypeHTTP3:
		default:
			return E.New("routefluent resolver group[", t.Tag(), "] primary resolver[", primaryTag, "] must be https or h3, got ", transport.Type())
		}
		primaries = append(primaries, &primaryState{
			tag:       primaryTag,
			transport: transport,
			state:     stateHealthy,
		})
	}
	t.primaries = primaries
	if t.fallbackEnabled {
		fallback, err := t.lookupFallback()
		if err != nil {
			return err
		}
		t.fallback = fallback
	}
	return nil
}

func (t *Transport) lookupFallback() (adapter.DNSTransport, error) {
	transport, loaded := t.manager.Transport(t.fallbackTag)
	if !loaded {
		return nil, E.New("routefluent resolver group[", t.Tag(), "] fallback resolver not found: ", t.fallbackTag)
	}
	if transport.Type() != C.DNSTypeLocal {
		return nil, E.New("routefluent resolver group[", t.Tag(), "] fallback resolver[", t.fallbackTag, "] must be local, got ", transport.Type())
	}
	return transport, nil
}

func (t *Transport) candidatePrimaries() []*primaryState {
	now := time.Now()
	t.access.Lock()
	defer t.access.Unlock()
	candidates := make([]*primaryState, 0, len(t.primaries))
	for _, primary := range t.primaries {
		if primary.state == stateUnhealthy && now.Before(primary.nextProbe) {
			continue
		}
		if primary.state == stateUnhealthy {
			primary.state = stateProbing
		}
		candidates = append(candidates, primary)
	}
	return candidates
}

func (t *Transport) markFailure(tag string, err error) {
	t.access.Lock()
	defer t.access.Unlock()
	primary := t.primaryByTag(tag)
	if primary == nil {
		return
	}
	primary.failures++
	primary.successes = 0
	primary.lastError = err.Error()
	if primary.failures >= t.failureThreshold {
		if primary.state != stateUnhealthy {
			t.logger.Warn("routefluent resolver group[", t.Tag(), "] primary resolver[", tag, "] unhealthy: ", err)
		}
		primary.state = stateUnhealthy
		primary.nextProbe = time.Now().Add(t.unhealthyCooldown)
		return
	}
	primary.state = stateSuspect
}

func (t *Transport) markProbeSuccess(tag string) {
	t.access.Lock()
	defer t.access.Unlock()
	primary := t.primaryByTag(tag)
	if primary == nil {
		return
	}
	primary.successes++
	primary.failures = 0
	if primary.successes >= t.recoverySuccessThreshold {
		if primary.state != stateHealthy {
			t.logger.Info("routefluent resolver group[", t.Tag(), "] primary resolver[", tag, "] recovered")
		}
		primary.state = stateHealthy
		primary.successes = 0
		primary.lastError = ""
		return
	}
	primary.state = stateUnhealthy
	primary.nextProbe = time.Now().Add(t.probeInterval)
}

func (t *Transport) markHealthy(tag string, recovered bool) {
	t.access.Lock()
	defer t.access.Unlock()
	primary := t.primaryByTag(tag)
	if primary == nil {
		return
	}
	if primary.state != stateHealthy && recovered {
		t.logger.Info("routefluent resolver group[", t.Tag(), "] primary resolver[", tag, "] recovered")
	}
	primary.state = stateHealthy
	primary.failures = 0
	primary.successes = 0
	primary.lastError = ""
}

func (t *Transport) primaryByTag(tag string) *primaryState {
	for _, primary := range t.primaries {
		if primary.tag == tag {
			return primary
		}
	}
	return nil
}

func (t *Transport) exchangeFallback(ctx context.Context, message *mDNS.Msg, reason string) (*mDNS.Msg, error) {
	t.access.Lock()
	fallback := t.fallback
	t.access.Unlock()
	if fallback == nil {
		return nil, E.New("routefluent resolver group[", t.Tag(), "] fallback resolver is not initialized")
	}
	t.logger.Warn("routefluent resolver group[", t.Tag(), "] selected local resolver[", fallback.Tag(), "], reason=", reason)
	response, err := fallback.Exchange(ctx, message.Copy())
	if err != nil {
		return nil, err
	}
	t.capFallbackTTL(response)
	return response, nil
}

func (t *Transport) capFallbackTTL(response *mDNS.Msg) {
	if response == nil || t.fallbackTTLCap <= 0 {
		return
	}
	ttl := uint32(t.fallbackTTLCap / time.Second)
	for _, recordList := range [][]mDNS.RR{response.Answer, response.Ns, response.Extra} {
		for _, record := range recordList {
			if record.Header().Rrtype == mDNS.TypeOPT {
				continue
			}
			if record.Header().Ttl == 0 || record.Header().Ttl > ttl {
				record.Header().Ttl = ttl
			}
		}
	}
}

func (t *Transport) probeLoop() {
	ticker := time.NewTicker(t.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.probeUnhealthy()
		case <-t.closed:
			return
		}
	}
}

func (t *Transport) probeUnhealthy() {
	targets := t.unhealthyPrimariesForProbe()
	if len(targets) == 0 {
		return
	}
	for _, primary := range targets {
		for _, domain := range t.probeDomains {
			if t.probePrimary(primary, domain) {
				t.markProbeSuccess(primary.tag)
				break
			}
		}
	}
}

func (t *Transport) unhealthyPrimariesForProbe() []*primaryState {
	now := time.Now()
	t.access.Lock()
	defer t.access.Unlock()
	var targets []*primaryState
	for _, primary := range t.primaries {
		if primary.state != stateUnhealthy || now.Before(primary.nextProbe) {
			continue
		}
		primary.state = stateProbing
		targets = append(targets, primary)
	}
	return targets
}

func (t *Transport) probePrimary(primary *primaryState, domain string) bool {
	if domain == "" {
		return false
	}
	timeout := t.probeInterval
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	message := new(mDNS.Msg)
	message.SetQuestion(mDNS.Fqdn(domain), mDNS.TypeA)
	response, err := primary.transport.Exchange(ctx, message)
	if err != nil {
		t.markFailure(primary.tag, err)
		return false
	}
	if response.Rcode == mDNS.RcodeServerFailure {
		t.markFailure(primary.tag, E.New("probe SERVFAIL"))
		return false
	}
	t.logger.Debug("routefluent resolver group[", t.Tag(), "] probe succeeded via primary resolver[", primary.tag, "] domain=", domain)
	return true
}
