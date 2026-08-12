package localapi

import "testing"

func TestMethodConnectionCancelIsAllowed(t *testing.T) {
	if !MethodConnectionCancel.valid() {
		t.Fatal("ConnectionCancel is not allowed by the local API")
	}
}

func TestPairStartTargetAllowsOnlyDiscoveredIDOrPrivateLiteral(t *testing.T) {
	for _, test := range []struct {
		params PairStartParams
		want   string
	}{
		{params: PairStartParams{Device: "discovered-id"}, want: "discovered-id"},
		{params: PairStartParams{Address: "192.168.1.20"}, want: "192.168.1.20"},
		{params: PairStartParams{Address: "10.0.0.20", Port: 49221}, want: "10.0.0.20"},
	} {
		got, err := test.params.Target()
		if err != nil || got != test.want {
			t.Fatalf("Target(%#v) = %q, %v; want %q", test.params, got, err, test.want)
		}
	}
	for _, params := range []PairStartParams{
		{Address: "example.test"}, {Address: "8.8.8.8"}, {Address: "127.0.0.1"},
		{Address: "192.168.1.20", Port: 49222}, {Device: "id", Address: "192.168.1.20"}, {Port: 49221},
	} {
		if _, err := params.Target(); err == nil {
			t.Fatalf("Target(%#v) accepted unsafe target", params)
		}
	}
}
