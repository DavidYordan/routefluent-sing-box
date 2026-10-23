package routefluentbinding

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sagernet/sing-box/dns"

	mDNS "github.com/miekg/dns"
)

func TestExplicitBindingReturnsOneCurrentIPv4AndRevisionEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "line-7.json")
	writeBinding(t, path, "explicit_ipv4", `"1.1.1.1"`, 4)
	transport := &Transport{
		TransportAdapter: dns.NewTransportAdapter("routefluent_ingress_binding", "ingress-binding-7", nil),
		bindingFile:      path, logicalDomain: "edge.example.com",
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
	if len(environment) != 4 || environment[1] != "binding_revision=4" {
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
		TransportAdapter: dns.NewTransportAdapter("routefluent_ingress_binding", "ingress-binding-7", nil),
		bindingFile:      path, logicalDomain: "edge.example.com",
	}
	request := new(mDNS.Msg)
	request.SetQuestion("other.example.com.", mDNS.TypeA)
	if _, err := transport.Exchange(context.Background(), request); err == nil {
		t.Fatal("an unrelated domain was answered from the explicit binding")
	}
}

func writeBinding(t *testing.T, path, mode, ipv4 string, revision uint64) {
	t.Helper()
	if mode == "resolver" {
		ipv4 = "null"
	}
	raw := []byte(`{"schema":"routefluent.device_line_ingress_binding.v1","tenant_id":3,"device_id":8,"line_asset_id":7,"endpoint_family_id":11,"logical_domain":"edge.example.com","mode":"` + mode + `","connect_ipv4":` + ipv4 + `,"binding_revision":` + strconv.FormatUint(revision, 10) + `,"last_server_choice_revision":1,"source":"server","updated_at":"2026-10-23T00:00:00Z"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
