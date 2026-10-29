package rfbinding

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/dns"

	mDNS "github.com/miekg/dns"
)

func TestExplicitBindingReturnsOneCurrentIPv4AndRevisionEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "line-7.json")
	writeBinding(t, path, "explicit_ipv4", `"1.1.1.1"`, 4)
	transport := &Transport{
		TransportAdapter: dns.NewTransportAdapter("rf_ingress_binding", "ingress-binding-7", nil),
		bindingFile:      path, logicalDomain: "edge.example.com", queryName: "edge.example.com",
	}
	request := new(mDNS.Msg)
	request.SetQuestion("edge.example.com.", mDNS.TypeA)
	response, err := transport.Exchange(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answer) != 1 || response.Answer[0].(*mDNS.A).A.String() != "1.1.1.1" {
		t.Fatalf("explicit binding answer differs: %#v", response.Answer)
	}
	environment := transport.Environment()
	if len(environment) != 5 || environment[1] != "binding_revision=4" || environment[4] != "query_name=edge.example.com" {
		t.Fatalf("binding revision is absent from cache environment: %v", environment)
	}
	writeBinding(t, path, "explicit_ipv4", `"8.8.8.8"`, 5)
	if transport.Environment()[1] != "binding_revision=5" {
		t.Fatalf("updated binding revision did not change cache environment: %v", transport.Environment())
	}
}

func TestExplicitBindingKeepsOtherDomainOutOfPrivateAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "line-7.json")
	writeBinding(t, path, "explicit_ipv4", `"1.1.1.1"`, 1)
	transport := &Transport{
		TransportAdapter: dns.NewTransportAdapter("rf_ingress_binding", "ingress-binding-7", nil),
		bindingFile:      path, logicalDomain: "edge.example.com", queryName: "edge.example.com",
	}
	request := new(mDNS.Msg)
	request.SetQuestion("other.example.com.", mDNS.TypeA)
	if _, err := transport.Exchange(context.Background(), request); err == nil {
		t.Fatal("an unrelated domain was answered from the explicit binding")
	}
}

func TestRetiredBindingSchemaIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "line-7.json")
	writeBinding(t, path, "explicit_ipv4", `"1.1.1.1"`, 1)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"schema":"rf.device_line_ingress_binding.v1"`), []byte(`"schema":"routefluent.device_line_ingress_binding.v1"`), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &Transport{
		TransportAdapter: dns.NewTransportAdapter("rf_ingress_binding", "ingress-binding-7", nil),
		bindingFile:      path, logicalDomain: "edge.example.com", queryName: "edge.example.com",
	}
	request := new(mDNS.Msg)
	request.SetQuestion("edge.example.com.", mDNS.TypeA)
	if _, err := transport.Exchange(context.Background(), request); err == nil {
		t.Fatal("retired binding schema was accepted")
	}
}

type recordingResolver struct {
	dns.TransportAdapter
	question string
}

func (r *recordingResolver) Start(adapter.StartStage) error { return nil }
func (r *recordingResolver) Close() error                   { return nil }
func (r *recordingResolver) Reset()                         {}
func (r *recordingResolver) Exchange(_ context.Context, request *mDNS.Msg) (*mDNS.Msg, error) {
	r.question = request.Question[0].Name
	response := new(mDNS.Msg)
	response.SetReply(request)
	record, err := mDNS.NewRR(request.Question[0].Name + " 60 IN A 192.0.2.10")
	if err != nil {
		return nil, err
	}
	response.Answer = []mDNS.RR{record}
	return response, nil
}
func (r *recordingResolver) ExchangeAsync(ctx context.Context, request *mDNS.Msg, callback func(*mDNS.Msg, error)) {
	response, err := r.Exchange(ctx, request)
	callback(response, err)
}

func TestResolverBindingQueriesAliasAndReturnsAnswerForLogicalDomain(t *testing.T) {
	resolver := &recordingResolver{TransportAdapter: dns.NewTransportAdapter("local", "resolver", nil)}
	transport := &Transport{
		TransportAdapter: dns.NewTransportAdapter("rf_ingress_binding", "ingress-binding-7", nil),
		logicalDomain:    "edge.example.com", queryName: "entry.example.net", resolver: resolver,
	}
	request := new(mDNS.Msg)
	request.SetQuestion("edge.example.com.", mDNS.TypeA)
	response, err := transport.Exchange(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.question != "entry.example.net." {
		t.Fatalf("underlying resolver received %q", resolver.question)
	}
	if len(response.Question) != 1 || response.Question[0].Name != "edge.example.com." || len(response.Answer) != 1 || response.Answer[0].Header().Name != "edge.example.com." {
		t.Fatalf("alias answer was not rebound to the logical domain: %#v", response)
	}
	if environment := transport.Environment(); len(environment) != 2 || environment[1] != "query_name=entry.example.net" {
		t.Fatalf("static resolver recipe is absent from cache environment: %v", environment)
	}
}

func writeBinding(t *testing.T, path, mode, ipv4 string, revision uint64) {
	t.Helper()
	if mode == "resolver" {
		ipv4 = "null"
	}
	raw := []byte(`{"schema":"rf.device_line_ingress_binding.v1","tenant_id":3,"device_id":8,"line_asset_id":7,"endpoint_family_id":11,"logical_domain":"edge.example.com","mode":"` + mode + `","connect_ipv4":` + ipv4 + `,"binding_revision":` + strconv.FormatUint(revision, 10) + `,"last_server_choice_revision":1,"source":"server","updated_at":"2026-10-23T00:00:00Z"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
