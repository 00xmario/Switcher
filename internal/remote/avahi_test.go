package remote

import "testing"

func TestAvahiBrowseFindsSwitcherHosts(t *testing.T) {
	out := []byte(`+;eth0;IPv6;Studio\032Mac;_switcher._tcp;local
+;eth0;IPv4;Studio\032Mac;_switcher._tcp;local
=;eth0;IPv6;Studio\032Mac;_switcher._tcp;local;studio.local;fe80::1;8788;"v=1" "fp=0123456789abcdef"
=;eth0;IPv4;Studio\032Mac;_switcher._tcp;local;studio.local;192.168.1.5;8788;"v=1" "fp=0123456789abcdef"
=;eth0;IPv4;Mario\226\128\153s\032K12;_switcher._tcp;local;k12.local;192.168.1.9;8788;"fp=fedcba9876543210" "v=1"
=;eth0;IPv4;Printer;_ipp._tcp;local;printer.local;192.168.1.2;631;""
=;eth0;IPv4;Broken;_switcher._tcp;local;broken.local;192.168.1.3;notaport;""
`)
	found := parseAvahi(out, "fedcba9876543210ffff")
	if len(found) != 2 {
		t.Fatalf("found %+v", found)
	}
	k12, studio := found[0], found[1]
	if k12.Name != "Mario’s K12" || k12.Address != "k12.local" || k12.Port != 8788 || !k12.Self {
		t.Errorf("own host: %+v", k12)
	}
	if studio.Name != "Studio Mac" || studio.Address != "studio.local" || studio.Self || studio.fp != "0123456789abcdef" {
		t.Errorf("other host: %+v", studio)
	}
}

func TestPrettyHostnameReadsMachineInfo(t *testing.T) {
	for in, want := range map[string]string{
		"PRETTY_HOSTNAME=\"Mario's K12\"\nICON_NAME=computer\n": "Mario's K12",
		"PRETTY_HOSTNAME=Studio\n":                              "Studio",
		"ICON_NAME=computer\n":                                  "",
	} {
		if got := prettyHostname([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
