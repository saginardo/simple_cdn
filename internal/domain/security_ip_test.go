package domain

import "testing"

func TestParseSecurityIP(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"8.8.8.8", "8.8.8.8"}, {" ::ffff:8.8.8.8 ", "8.8.8.8"},
		{"2001:4860:4860:0000:0000:0000:0000:8888", "2001:4860:4860::8888"},
		{"2001:db8::1", "2001:db8::1"},
		{"10.0.0.1", ""}, {"::ffff:192.168.1.1", ""}, {"127.0.0.1", ""},
		{"::1", ""}, {"::", ""}, {"fc00::1", ""}, {"fe80::1", ""},
		{"ff02::1", ""}, {"2001:db8::1%eth0", ""}, {"[2001:db8::1]", ""},
		{"2001:db8::/32", ""}, {"invalid", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			address, err := ParseSecurityIP(test.input)
			if test.want == "" {
				if err == nil {
					t.Fatalf("accepted unsafe address %q", test.input)
				}
			} else if err != nil || address.String() != test.want {
				t.Fatalf("got %v, %v; want %s", address, err, test.want)
			}
		})
	}
}
