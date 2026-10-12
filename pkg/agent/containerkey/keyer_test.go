package containerkey

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeResolver answers from a table of destinations, or with err.
type fakeResolver struct {
	at    map[string]Container // "ip:port" -> container
	err   error
	asked int
}

func (f *fakeResolver) Resolve(_ context.Context, dst netip.AddrPort) (Container, bool, error) {
	f.asked++
	if f.err != nil {
		return Container{}, false, f.err
	}
	c, ok := f.at[dst.String()]
	return c, ok, nil
}

func (f *fakeResolver) Endpoint() string { return "unix:///fake.sock" }

var (
	pgSuite = Container{ID: "suite", Image: "postgres:16", Port: 5432}
	pgA     = Container{ID: "a", Image: "postgres:16", Port: 5432}
	pgB     = Container{ID: "b", Image: "postgres:16", Port: 5432}
	redis   = Container{ID: "r", Image: "redis:7", Port: 6379}
)

func newFake() *fakeResolver {
	return &fakeResolver{at: map[string]Container{
		"127.0.0.1:40001": pgSuite,
		"127.0.0.1:40002": pgA,
		"127.0.0.1:40003": pgB,
		"172.17.0.9:5432": pgB, // pgB at its own IP
		"127.0.0.1:40004": redis,
		"[::1]:40002":     pgA,
	}}
}

func name(t *testing.T, k *Keyer, connID, dst string) string {
	t.Helper()
	_, key, _ := k.Name(context.Background(), zap.NewNop(), connID, dst)
	return key
}

func TestKeyerNumbersAnImagesContainersByFirstConnection(t *testing.T) {
	k := NewKeyer(newFake())
	if got := name(t, k, "1", "127.0.0.1:40002"); got != "container:postgres:16:5432#1" {
		t.Fatalf("first container = %q", got)
	}
	if got := name(t, k, "2", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("second container = %q", got)
	}
	// Another connection to the first one, by another address of it, keeps
	// its number; so does the second at its own IP.
	if got := name(t, k, "3", "[::1]:40002"); got != "container:postgres:16:5432#1" {
		t.Fatalf("first container again = %q", got)
	}
	if got := name(t, k, "4", "172.17.0.9:5432"); got != "container:postgres:16:5432#2" {
		t.Fatalf("second container at its IP = %q", got)
	}
	// Each image counts its own containers.
	if got := name(t, k, "5", "127.0.0.1:40004"); got != "container:redis:7:6379#1" {
		t.Fatalf("other image = %q", got)
	}
}

func TestKeyerNamesNothingThatIsNotAContainer(t *testing.T) {
	k := NewKeyer(newFake())
	for _, dst := range []string{"127.0.0.1:9", "not-an-address", "", "127.0.0.1:99999"} {
		if c, key, ok := k.Name(context.Background(), zap.NewNop(), "1", dst); ok || key != "" || c != (Container{}) {
			t.Fatalf("Name(%q) = %+v, %q, %v", dst, c, key, ok)
		}
	}
	m := &models.Mock{ConnectionID: "1"}
	k.Stamp(m)
	if m.Spec.Metadata != nil {
		t.Fatalf("a mock of a connection with no key was stamped: %v", m.Spec.Metadata)
	}
	if _, _, ok := NewKeyer(nil).Name(context.Background(), zap.NewNop(), "1", "127.0.0.1:40002"); ok {
		t.Fatal("a keyer with no resolver named a destination")
	}
}

// A test's own containers are numbered within the test, so each test that
// starts its own container gets #1, as it does when it runs alone; a
// container the suite set up keeps its number inside every test, and a test's
// own container of the same image is numbered after it.
func TestKeyerNumbersEachTestOnItsOwnAfterTheRun(t *testing.T) {
	k := NewKeyer(newFake())
	if got := name(t, k, "boot", "127.0.0.1:40001"); got != "container:postgres:16:5432#1" {
		t.Fatalf("suite container = %q", got)
	}

	k.BeginTest()
	if got := name(t, k, "t1-own", "127.0.0.1:40002"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 1's own container = %q", got)
	}
	if got := name(t, k, "t1-suite", "127.0.0.1:40001"); got != "container:postgres:16:5432#1" {
		t.Fatalf("suite container inside test 1 = %q", got)
	}
	k.EndTest()

	k.BeginTest()
	if got := name(t, k, "t2-own", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 2's own container = %q, want the number test 1's had", got)
	}
	k.EndTest()

	// Outside tests the run's numbering goes on where it was: the containers
	// tests saw are new to it.
	if got := name(t, k, "after", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("run scope after the tests = %q", got)
	}

	// The same test run alone numbers its container the same.
	alone := NewKeyer(newFake())
	_ = name(t, alone, "boot", "127.0.0.1:40001")
	alone.BeginTest()
	if got := name(t, alone, "t2-own", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 2 run alone = %q", got)
	}
}

func TestKeyerKeepsAConnectionsKeyAcrossScopes(t *testing.T) {
	k := NewKeyer(newFake())
	k.BeginTest()
	_ = name(t, k, "pooled", "127.0.0.1:40002")
	k.EndTest()
	k.BeginTest()
	_ = name(t, k, "new", "127.0.0.1:40003")
	k.EndTest()

	pooled := &models.Mock{ConnectionID: "pooled"}
	k.Stamp(pooled)
	if got := pooled.Spec.Metadata[MetadataKey]; got != "container:postgres:16:5432#1" {
		t.Fatalf("pooled connection's mock = %q", got)
	}
}

func TestKeyerStampsByConnection(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "7", "127.0.0.1:40004")

	byID := &models.Mock{ConnectionID: "7"}
	k.Stamp(byID)
	if got := byID.Spec.Metadata[MetadataKey]; got != "container:redis:7:6379#1" {
		t.Fatalf("by ConnectionID = %q", got)
	}
	// HTTP's recorder carries the connection in its metadata instead.
	byMeta := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{"connID": "7", "operation": "GET"}}}
	k.Stamp(byMeta)
	if got := byMeta.Spec.Metadata[MetadataKey]; got != "container:redis:7:6379#1" || byMeta.Spec.Metadata["operation"] != "GET" {
		t.Fatalf("by metadata connID = %v", byMeta.Spec.Metadata)
	}
	other := &models.Mock{ConnectionID: "8"}
	k.Stamp(other)
	if other.Spec.Metadata != nil {
		t.Fatalf("another connection's mock was stamped: %v", other.Spec.Metadata)
	}
	k.Stamp(nil)

	k.Reset()
	afterReset := &models.Mock{ConnectionID: "7"}
	k.Stamp(afterReset)
	if afterReset.Spec.Metadata != nil {
		t.Fatalf("a reset keyer still stamped: %v", afterReset.Spec.Metadata)
	}
	if got := name(t, k, "9", "127.0.0.1:40003"); got != "container:postgres:16:5432#1" {
		t.Fatalf("numbering after Reset = %q", got)
	}
}

func TestKeyerSaysOncePerSessionThatDockerCannotBeAsked(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	f := &fakeResolver{err: errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")}
	k := NewKeyer(f)
	for i := 0; i < 3; i++ {
		if _, key, ok := k.Name(context.Background(), logger, "1", "127.0.0.1:40002"); ok || key != "" {
			t.Fatalf("named %q with Docker unreachable", key)
		}
	}
	if logs.Len() != 1 {
		t.Fatalf("warned %d times, want once", logs.Len())
	}
	e := logs.All()[0]
	if e.ContextMap()["dockerEndpoint"] != "unix:///fake.sock" || e.ContextMap()["destination"] != "127.0.0.1:40002" {
		t.Fatalf("the warning does not name the endpoint and destination: %v", e.ContextMap())
	}
	k.Reset()
	_, _, _ = k.Name(context.Background(), logger, "1", "127.0.0.1:40002")
	if logs.Len() != 2 {
		t.Fatalf("a new session did not warn again: %d", logs.Len())
	}
	// A connection whose context ended (the agent stopping) says nothing.
	k.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = k.Name(ctx, logger, "1", "127.0.0.1:40002")
	if logs.Len() != 2 {
		t.Fatalf("a cancelled connection warned: %d", logs.Len())
	}
}

func TestParseAddrPort(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:80":        "127.0.0.1:80",
		"[::1]:80":            "[::1]:80",
		"[::ffff:10.0.0.1]:9": "10.0.0.1:9",
	} {
		got, err := parseAddrPort(in)
		if err != nil || got.String() != want {
			t.Fatalf("parseAddrPort(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "127.0.0.1", "host:80", "127.0.0.1:x"} {
		if _, err := parseAddrPort(in); err == nil {
			t.Fatalf("parseAddrPort(%q) read it", in)
		}
	}
}

// A disabled keyer (a docker-mode agent, which has no Docker socket) asks
// nothing and names nothing, and says nothing about it.
func TestKeyerDisabledAsksNothing(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	f := newFake()
	k := NewKeyer(f)
	k.Disable()
	if _, key, ok := k.Name(context.Background(), zap.New(core), "1", "127.0.0.1:40002"); ok || key != "" {
		t.Fatalf("a disabled keyer named %q", key)
	}
	if f.asked != 0 || logs.Len() != 0 {
		t.Fatalf("a disabled keyer asked %d times and logged %d lines", f.asked, logs.Len())
	}
}
