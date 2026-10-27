package sip

import "testing"

func TestIdentityOfCanonicalizesRoutingFieldsAndKeepsExactUsername(t *testing.T) {
	identity, err := IdentityOf(Configuration{
		Host: "PBX.Branch.Example.", Port: 5060, Transport: "UDP",
		Realm: " branch-realm ", Username: "Agent-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.Host != "pbx.branch.example" || identity.Transport != TransportUDP ||
		identity.Realm != "branch-realm" || identity.Username != "Agent-01" {
		t.Fatalf("unexpected canonical identity: %+v", identity)
	}
}

func TestIdentityOfSeparatesTheSameUsernameAcrossPBXBoundaries(t *testing.T) {
	baseline := Configuration{
		Host: "192.0.2.10", Port: 5060, Transport: TransportUDP, Realm: "a", Username: "100",
	}
	first, err := IdentityOf(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for name, configuration := range map[string]Configuration{
		"host":      {Host: "192.0.2.11", Port: 5060, Transport: TransportUDP, Realm: "a", Username: "100"},
		"port":      {Host: "192.0.2.10", Port: 5070, Transport: TransportUDP, Realm: "a", Username: "100"},
		"transport": {Host: "192.0.2.10", Port: 5060, Transport: TransportTCP, Realm: "a", Username: "100"},
		"realm":     {Host: "192.0.2.10", Port: 5060, Transport: TransportUDP, Realm: "b", Username: "100"},
		"username":  {Host: "192.0.2.10", Port: 5060, Transport: TransportUDP, Realm: "a", Username: "100A"},
	} {
		t.Run(name, func(t *testing.T) {
			second, secondErr := IdentityOf(configuration)
			if secondErr != nil {
				t.Fatal(secondErr)
			}
			if first == second {
				t.Fatalf("different %s collapsed into one SIP identity", name)
			}
		})
	}
}

func TestIdentityOfRejectsUnsupportedHosts(t *testing.T) {
	for _, host := range []string{
		"2001:db8::1", "https://pbx.example", "pbx.example:5060", "pbx_1.example", "สาขา.example",
	} {
		t.Run(host, func(t *testing.T) {
			_, err := IdentityOf(Configuration{
				Host: host, Port: 5060, Transport: TransportUDP, Username: "100",
			})
			if err == nil {
				t.Fatalf("unsupported host %q was accepted", host)
			}
		})
	}
}
