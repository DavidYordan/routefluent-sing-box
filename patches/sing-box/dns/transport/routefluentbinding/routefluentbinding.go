package routefluentbinding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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
	bindingSchema = "routefluent.device_line_ingress_binding.v1"
	modeResolver  = "resolver"
	modeExplicit  = "explicit_ipv4"
	maximumBytes  = 64 << 10
	answerTTL     = 1
)

var _ adapter.DNSTransport = (*Transport)(nil)
var _ adapter.DNSTransportWithEnvironment = (*Transport)(nil)

func RegisterTransport(registry *boxdns.TransportRegistry) {
	boxdns.RegisterTransport[option.RouteFluentIngressBindingDNSServerOptions](registry, C.DNSTypeRouteFluentIngressBinding, NewTransport)
}

type bindingCurrent struct {
	Schema                   string  `json:"schema"`
	TenantID                 uint64  `json:"tenant_id"`
	DeviceID                 uint64  `json:"device_id"`
	LineAssetID              uint64  `json:"line_asset_id"`
	EndpointFamilyID         uint64  `json:"endpoint_family_id"`
	LogicalDomain            string  `json:"logical_domain"`
	Mode                     string  `json:"mode"`
	ConnectIPv4              *string `json:"connect_ipv4"`
	BindingRevision          uint64  `json:"binding_revision"`
	LastServerChoiceRevision uint64  `json:"last_server_choice_revision"`
	Source                   string  `json:"source"`
	UpdatedAt                string  `json:"updated_at"`
}

type Transport struct {
	boxdns.TransportAdapter

	manager       adapter.DNSTransportManager
	bindingFile   string
	logicalDomain string
	queryName     string
	resolverTag   string

	access   sync.RWMutex
	resolver adapter.DNSTransport
}

func NewTransport(ctx context.Context, _ log.ContextLogger, tag string, options option.RouteFluentIngressBindingDNSServerOptions) (adapter.DNSTransport, error) {
	if tag == "" {
		return nil, E.New("routefluent ingress binding tag is required")
	}
	bindingFile := strings.TrimSpace(options.BindingFile)
	if bindingFile != "" && (!filepath.IsAbs(bindingFile) || filepath.Clean(bindingFile) != bindingFile) {
		return nil, E.New("routefluent ingress binding_file must be an absolute clean path")
	}
	domain := strings.TrimSpace(options.LogicalDomain)
	if !canonicalDomain(domain) {
		return nil, E.New("routefluent ingress logical_domain is invalid")
	}
	queryName := strings.TrimSpace(options.QueryName)
	if queryName == "" {
		queryName = domain
	}
	if !canonicalDomain(queryName) {
		return nil, E.New("routefluent ingress query_name is invalid")
	}
	resolverTag := strings.TrimSpace(options.Resolver)
	if resolverTag == "" || resolverTag == tag {
		return nil, E.New("routefluent ingress binding requires another resolver transport")
	}
	manager := service.FromContext[adapter.DNSTransportManager](ctx)
	if manager == nil {
		return nil, E.New("missing DNS transport manager in context")
	}
	return &Transport{
		TransportAdapter: boxdns.NewTransportAdapter(C.DNSTypeRouteFluentIngressBinding, tag, []string{resolverTag}),
		manager:          manager, bindingFile: bindingFile, logicalDomain: domain, queryName: queryName, resolverTag: resolverTag,
	}, nil
}

func (t *Transport) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	resolver, loaded := t.manager.Transport(t.resolverTag)
	if !loaded {
		return E.New("routefluent ingress binding resolver not found: ", t.resolverTag)
	}
	t.access.Lock()
	t.resolver = resolver
	t.access.Unlock()
	return nil
}

func (t *Transport) Close() error { return nil }

func (t *Transport) Reset() {
	t.access.RLock()
	resolver := t.resolver
	t.access.RUnlock()
	if resolver != nil {
		resolver.Reset()
	}
}

func (t *Transport) Environment() []string {
	if t.bindingFile == "" {
		return []string{"mode=resolver", "query_name=" + t.queryName}
	}
	binding, err := t.loadBinding()
	if err != nil {
		return []string{"binding=invalid"}
	}
	ipv4 := ""
	if binding.ConnectIPv4 != nil {
		ipv4 = *binding.ConnectIPv4
	}
	return []string{
		"line_asset=" + strconv.FormatUint(binding.LineAssetID, 10),
		"binding_revision=" + strconv.FormatUint(binding.BindingRevision, 10),
		"mode=" + binding.Mode,
		"connect_ipv4=" + ipv4,
		"query_name=" + t.queryName,
	}
}

func (t *Transport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	if message == nil || len(message.Question) != 1 {
		return nil, E.New("routefluent ingress binding requires one DNS question")
	}
	var binding bindingCurrent
	if t.bindingFile != "" {
		var err error
		binding, err = t.loadBinding()
		if err != nil {
			return nil, err
		}
	}
	question := message.Question[0]
	if strings.TrimSuffix(strings.ToLower(question.Name), ".") != t.logicalDomain {
		return nil, E.New("routefluent ingress binding received an unrelated domain")
	}
	if t.bindingFile == "" || binding.Mode == modeResolver {
		return t.exchangeResolver(ctx, message)
	}
	response := new(mDNS.Msg)
	response.SetReply(message)
	response.RecursionAvailable = true
	if question.Qtype == mDNS.TypeA || question.Qtype == mDNS.TypeANY {
		record, err := mDNS.NewRR(fmt.Sprintf("%s %d IN A %s", mDNS.Fqdn(t.logicalDomain), answerTTL, *binding.ConnectIPv4))
		if err != nil {
			return nil, err
		}
		response.Answer = append(response.Answer, record)
	}
	return response, nil
}

func (t *Transport) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(response *mDNS.Msg, err error)) {
	go func() {
		response, err := t.Exchange(ctx, message)
		callback(response, err)
	}()
}

func (t *Transport) exchangeResolver(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	t.access.RLock()
	resolver := t.resolver
	t.access.RUnlock()
	if resolver == nil {
		return nil, E.New("routefluent ingress binding resolver is not initialized")
	}
	request := message.Copy()
	request.Question[0].Name = mDNS.Fqdn(t.queryName)
	response, err := resolver.Exchange(ctx, request)
	if err != nil || response == nil || t.queryName == t.logicalDomain {
		return response, err
	}
	response = response.Copy()
	response.Id = message.Id
	response.Question = append([]mDNS.Question(nil), message.Question...)
	queryFQDN := mDNS.Fqdn(t.queryName)
	logicalFQDN := mDNS.Fqdn(t.logicalDomain)
	for _, answer := range response.Answer {
		if strings.EqualFold(answer.Header().Name, queryFQDN) {
			answer.Header().Name = logicalFQDN
		}
	}
	return response, nil
}

func (t *Transport) loadBinding() (bindingCurrent, error) {
	info, err := os.Lstat(t.bindingFile)
	if err != nil {
		return bindingCurrent{}, E.Cause(err, "read routefluent ingress binding")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximumBytes {
		return bindingCurrent{}, E.New("routefluent ingress binding file is invalid")
	}
	raw, err := os.ReadFile(t.bindingFile)
	if err != nil {
		return bindingCurrent{}, E.Cause(err, "read routefluent ingress binding")
	}
	var binding bindingCurrent
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return bindingCurrent{}, E.Cause(err, "decode routefluent ingress binding")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return bindingCurrent{}, E.New("routefluent ingress binding contains trailing JSON")
	}
	if binding.Schema != bindingSchema || binding.TenantID == 0 || binding.DeviceID == 0 || binding.LineAssetID == 0 || binding.EndpointFamilyID == 0 || binding.BindingRevision == 0 || binding.LogicalDomain != t.logicalDomain {
		return bindingCurrent{}, E.New("routefluent ingress binding identity is invalid")
	}
	switch binding.Mode {
	case modeResolver:
		if binding.ConnectIPv4 != nil {
			return bindingCurrent{}, E.New("resolver binding contains connect_ipv4")
		}
	case modeExplicit:
		if binding.ConnectIPv4 == nil {
			return bindingCurrent{}, E.New("explicit ingress binding is missing connect_ipv4")
		}
		address, err := netip.ParseAddr(*binding.ConnectIPv4)
		if err != nil || !address.Is4() || address.String() != *binding.ConnectIPv4 {
			return bindingCurrent{}, E.New("explicit ingress binding connect_ipv4 is invalid")
		}
	default:
		return bindingCurrent{}, E.New("routefluent ingress binding mode is invalid")
	}
	return binding, nil
}

func canonicalDomain(value string) bool {
	if value == "" || value != strings.ToLower(value) || strings.HasSuffix(value, ".") || len(value) > 253 {
		return false
	}
	_, valid := mDNS.IsDomainName(mDNS.Fqdn(value))
	return valid
}
