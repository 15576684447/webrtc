package ice_support_remonination

import (
	"net"
	"testing"
)

func TestNetworkTypeParsing_Success(t *testing.T) {
	ipv4 := net.ParseIP("192.168.0.1")
	ipv6 := net.ParseIP("fe80::a3:6ff:fec4:5454")

	for _, test := range []struct {
		name      string
		inNetwork string
		inIP      net.IP
		expected  NetworkType
	}{
		{
			"lowercase UDP4",
			"udp",
			ipv4,
			NetworkTypeUDP4,
		},
		{
			"uppercase UDP4",
			"UDP",
			ipv4,
			NetworkTypeUDP4,
		},
		{
			"lowercase UDP6",
			"udp",
			ipv6,
			NetworkTypeUDP6,
		},
		{
			"uppercase UDP6",
			"UDP",
			ipv6,
			NetworkTypeUDP6,
		},
	} {
		actual, err := determineNetworkType(test.inNetwork, test.inIP)
		if err != nil {
			t.Errorf("NetworkTypeParsing failed: %v", err)
		}
		if actual != test.expected {
			t.Errorf("NetworkTypeParsing: '%s' -- input:%s expected:%s actual:%s",
				test.name, test.inNetwork, test.expected, actual)
		}
	}
}

func TestNetworkTypeParsing_Failure(t *testing.T) {
	ipv6 := net.ParseIP("fe80::a3:6ff:fec4:5454")

	for _, test := range []struct {
		name      string
		inNetwork string
		inIP      net.IP
	}{
		{
			"invalid network",
			"junkNetwork",
			ipv6,
		},
	} {
		actual, err := determineNetworkType(test.inNetwork, test.inIP)
		if err == nil {
			t.Errorf("NetworkTypeParsing should fail: '%s' -- input:%s actual:%s",
				test.name, test.inNetwork, actual)
		}
	}
}

func TestNetworkType_IsReliable(t *testing.T) {
	tests := []struct {
		name string
		t    NetworkType
		want bool
	}{
		{"1", NetworkTypeTCP4, true},
		{"2", NetworkTypeTCP6, true},
		{"3", NetworkTypeUDP4, false},
		{"4", NetworkTypeUDP6, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.t.IsReliable(); got != tt.want {
				t.Errorf("IsReliable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetworkType_NetworkShort(t *testing.T) {
	tests := []struct {
		name string
		t    NetworkType
		want string
	}{
		{"1", NetworkTypeTCP4, "tcp"},
		{"2", NetworkTypeTCP6, "tcp"},
		{"3", NetworkTypeUDP4, "udp"},
		{"4", NetworkTypeUDP6, "udp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.t.NetworkShort(); got != tt.want {
				t.Errorf("NetworkShort() = %v, want %v", got, tt.want)
			}
		})
	}
}
