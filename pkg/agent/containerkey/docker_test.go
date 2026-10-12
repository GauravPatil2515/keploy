package containerkey

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

type fakeDocker struct {
	list     []container.Summary
	images   map[string]string // container ID -> Config.Image
	listErr  error
	lists    int
	inspects int
}

func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	f.lists++
	return f.list, f.listErr
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	f.inspects++
	image, ok := f.images[id]
	if !ok {
		return container.InspectResponse{}, errors.New("no such container: " + id)
	}
	return container.InspectResponse{Config: &container.Config{Image: image}}, nil
}

func (f *fakeDocker) DaemonHost() string { return "unix:///fake.sock" }

func onNetwork(ip, ip6 string) *container.NetworkSettingsSummary {
	return &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
		"bridge": {IPAddress: ip, GlobalIPv6Address: ip6},
	}}
}

// hostAt is a resolver whose host has the addresses given.
func hostAt(api dockerAPI, addrs ...string) *dockerResolver {
	r := &dockerResolver{api: api}
	r.hostAddrs = func() ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	return r
}

func resolve(t *testing.T, r *dockerResolver, dst string) (Container, bool) {
	t.Helper()
	c, ok, err := r.Resolve(context.Background(), netip.MustParseAddrPort(dst))
	if err != nil {
		t.Fatalf("Resolve(%s): %v", dst, err)
	}
	return c, ok
}

func testcontainers() *fakeDocker {
	return &fakeDocker{
		list: []container.Summary{
			{
				ID: "pg", Image: "sha256:feed", // the tag moved since: the list shows the image ID
				Ports: []container.Port{
					{IP: "0.0.0.0", PrivatePort: 5432, PublicPort: 49153, Type: "tcp"},
					{IP: "::", PrivatePort: 5432, PublicPort: 49153, Type: "tcp"},
					{IP: "0.0.0.0", PrivatePort: 5432, PublicPort: 49160, Type: "udp"},
				},
				NetworkSettings: onNetwork("172.17.0.2", "fd00::2"),
			},
			{
				ID: "ls", Image: "localstack/localstack:3.8",
				Ports:           []container.Port{{IP: "127.0.0.1", PrivatePort: 4566, PublicPort: 49200, Type: "tcp"}},
				NetworkSettings: onNetwork("172.17.0.3", ""),
			},
			{
				ID: "private", Image: "redis:7",
				Ports:           []container.Port{{IP: "10.0.0.5", PrivatePort: 6379, PublicPort: 49300, Type: "tcp"}},
				NetworkSettings: &container.NetworkSettingsSummary{},
			},
		},
		images: map[string]string{"pg": "docker.io/library/postgres:16-alpine", "ls": "localstack/localstack:3.8", "private": "redis"},
	}
}

func TestResolveAPublishedPortOnThisHost(t *testing.T) {
	api := testcontainers()
	r := hostAt(api, "10.0.0.5", "192.168.1.20")
	for dst, want := range map[string]Container{
		"127.0.0.1:49153":    {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"[::1]:49153":        {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"192.168.1.20:49153": {ID: "pg", Image: "postgres:16-alpine", Port: 5432}, // all addresses
		"127.0.0.1:49200":    {ID: "ls", Image: "localstack/localstack:3.8", Port: 4566},
		"10.0.0.5:49300":     {ID: "private", Image: "redis:latest", Port: 6379}, // bound to that address
	} {
		got, ok := resolve(t, r, dst)
		if !ok || got != want {
			t.Errorf("Resolve(%s) = %+v, %v; want %+v", dst, got, ok, want)
		}
	}
	// Each container's image is read once.
	if api.inspects != 3 {
		t.Fatalf("inspected %d times, want once per container", api.inspects)
	}
}

func TestResolveAContainersOwnIP(t *testing.T) {
	r := hostAt(testcontainers())
	for dst, want := range map[string]Container{
		"172.17.0.2:5432": {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"[fd00::2]:5432":  {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"172.17.0.3:4566": {ID: "ls", Image: "localstack/localstack:3.8", Port: 4566},
	} {
		got, ok := resolve(t, r, dst)
		if !ok || got != want {
			t.Errorf("Resolve(%s) = %+v, %v; want %+v", dst, got, ok, want)
		}
	}
}

func TestResolveLeavesEverythingElse(t *testing.T) {
	api := testcontainers()
	r := hostAt(api, "192.168.1.20")
	for _, dst := range []string{
		"127.0.0.1:5432",     // no container publishes it
		"127.0.0.1:49160",    // published for UDP only
		"192.168.1.20:49200", // published on loopback only
		"10.0.0.9:49153",     // another machine with the same port number
		"172.17.0.4:5432",    // no container has that IP
		"127.0.0.1:49300",    // published on 10.0.0.5 only
	} {
		if got, ok := resolve(t, r, dst); ok {
			t.Errorf("Resolve(%s) = %+v, want no container", dst, got)
		}
	}
	// A public address is not asked about at all.
	lists := api.lists
	if _, ok := resolve(t, r, "203.0.113.7:49153"); ok {
		t.Fatal("a public address resolved to a container")
	}
	if api.lists != lists {
		t.Fatal("a public address was looked up in Docker")
	}
}

func TestResolveFallsBackToTheListsImage(t *testing.T) {
	api := testcontainers()
	api.images = map[string]string{"ls": ""}
	r := hostAt(api)
	if got, ok := resolve(t, r, "127.0.0.1:49200"); !ok || got.Image != "localstack/localstack:3.8" {
		t.Fatalf("Resolve = %+v, %v", got, ok)
	}
}

func TestResolveSaysWhenDockerCannotBeAsked(t *testing.T) {
	api := &fakeDocker{listErr: errors.New("Cannot connect to the Docker daemon")}
	if _, ok, err := hostAt(api).Resolve(context.Background(), netip.MustParseAddrPort("127.0.0.1:49153")); ok || err == nil {
		t.Fatalf("Resolve = %v, %v; want an error", ok, err)
	}
	// An inspect that fails is an error too, never a key without its image.
	api = testcontainers()
	api.images = nil
	if _, ok, err := hostAt(api).Resolve(context.Background(), netip.MustParseAddrPort("127.0.0.1:49153")); ok || err == nil {
		t.Fatalf("Resolve = %v, %v; want an error", ok, err)
	}
	// A client that cannot be built.
	r := &dockerResolver{err: errors.New("bad DOCKER_HOST"), hostAddrs: interfaceAddrs}
	if _, ok, err := r.Resolve(context.Background(), netip.MustParseAddrPort("127.0.0.1:49153")); ok || err == nil {
		t.Fatalf("Resolve = %v, %v; want an error", ok, err)
	}
}

func TestSetDockerHostPointsTheClientAtIt(t *testing.T) {
	r := newDockerResolver()
	saved := Default.res
	Default.res = r
	t.Cleanup(func() { Default.res = saved })
	SetDockerHost("unix:///run/user/1000/docker.sock")
	if got := r.Endpoint(); got != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("Endpoint() = %q", got)
	}
}

func TestAgentArgsCarryDockerHostOnlyUnderTheSwitch(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("KEPLOY_PROVEN", "")
	if got := AgentArgs(); got != nil {
		t.Fatalf("switch off: AgentArgs() = %q", got)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := AgentArgs(); len(got) != 1 || got[0] != "--docker-host=unix:///run/user/1000/docker.sock" {
		t.Fatalf("switch on: AgentArgs() = %q", got)
	}
	t.Setenv("DOCKER_HOST", "")
	if got := AgentArgs(); got != nil {
		t.Fatalf("no DOCKER_HOST: AgentArgs() = %q", got)
	}
}
