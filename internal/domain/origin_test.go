package domain

import (
	"math"
	"testing"
	"time"
)

func TestValidOriginPoolAndProbeStatus(t *testing.T) {
	pool := OriginPool{
		ID: "0123456789abcdef01234567", Address: "203.0.113.10:443", Scheme: "https",
		HostHeader: "origin.example.test", TLSServerName: "origin.example.test",
		ConfigPath:           "/opt/cdn-edge/config/nginx/origin-pools/0123456789abcdef01234567.conf",
		KeepaliveConnections: 32, References: []OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
	}
	if !ValidOriginPool(pool) {
		t.Fatalf("valid origin pool was rejected: %#v", pool)
	}
	http2Pool := pool
	http2Pool.HTTPVersion = OriginHTTPVersionHTTP2
	if !ValidOriginPool(http2Pool) {
		t.Fatalf("valid HTTP/2 origin pool was rejected: %#v", http2Pool)
	}
	http2Pool.Scheme = "http"
	if ValidOriginPool(http2Pool) {
		t.Fatal("TLS HTTP/2 mode was accepted for a cleartext HTTP pool")
	}
	invalid := pool
	invalid.ConfigPath = "/opt/cdn-edge/config/nginx/origin-pools/../outside.conf"
	if ValidOriginPool(invalid) {
		t.Fatal("unclean origin pool path was accepted")
	}
	invalid = pool
	invalid.References = append(invalid.References, invalid.References[0])
	if ValidOriginPool(invalid) {
		t.Fatal("duplicate origin pool reference was accepted")
	}
	checkedAt := time.Now().UTC()
	status := OriginProbeStatus{
		PoolID: pool.ID, Address: pool.Address, Scheme: pool.Scheme,
		HTTPVersion:          OriginHTTPVersionHTTP2,
		KeepaliveConnections: pool.KeepaliveConnections, References: pool.References,
		Healthy: true, CircuitState: OriginCircuitClosed, CheckedAt: checkedAt,
		ServiceProbe: &OriginProbeSample{
			Healthy: true, ConnectionReused: true, HeaderMS: 8, TotalMS: 9,
			HTTPStatus: 204, CheckedAt: checkedAt,
		},
	}
	if !ValidOriginProbeStatus(status) {
		t.Fatalf("valid origin status was rejected: %#v", status)
	}
	establishedConnections := int64(3)
	status.EstablishedConnections = &establishedConnections
	if !ValidOriginProbeStatus(status) {
		t.Fatalf("origin status with a connection count was rejected: %#v", status)
	}
	invalidConnectionCount := int64(-1)
	status.EstablishedConnections = &invalidConnectionCount
	if ValidOriginProbeStatus(status) {
		t.Fatal("negative origin connection count was accepted")
	}
	status.EstablishedConnections = &establishedConnections
	status.ServiceProbe.TotalMS = math.NaN()
	if ValidOriginProbeStatus(status) {
		t.Fatal("NaN origin timing was accepted")
	}
	status.ServiceProbe.TotalMS = 9
	status.ColdProbe = &OriginProbeSample{Healthy: true, ConnectionReused: true, TotalMS: 2, CheckedAt: checkedAt}
	if ValidOriginProbeStatus(status) {
		t.Fatal("cold probe connection reuse was accepted")
	}
}
