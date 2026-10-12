package containerkey

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// dockerAPI is the part of the Engine API the resolver reads.
type dockerAPI interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	DaemonHost() string
}

// dockerResolver finds the running container behind a destination through the
// Docker API, at the moment the connection is accepted: a container's
// published port on this host, or a container's own IP.
type dockerResolver struct {
	mu   sync.Mutex
	host string // the endpoint SetDockerHost gave; "" for the environment's
	api  dockerAPI
	err  error // why the client could not be built
	// images is each container's image reference, which never changes for a
	// container, so it is read once per container.
	images map[string]string
	// hostAddrs lists this host's own addresses: a published port is reached
	// at any of them. A variable so a test can give its own.
	hostAddrs func() ([]netip.Addr, error)
}

func newDockerResolver() *dockerResolver {
	return &dockerResolver{hostAddrs: interfaceAddrs}
}

// SetDockerHost points the agent's resolver at the Docker endpoint its keploy
// client uses (DOCKER_HOST), which the agent does not inherit: sudo drops the
// environment. Called once, at the agent's start, before any connection.
func SetDockerHost(host string) {
	if r, ok := Default.res.(*dockerResolver); ok {
		r.mu.Lock()
		r.host = host
		r.mu.Unlock()
	}
}

// client is the Docker API client, built on first use.
func (r *dockerResolver) client() (dockerAPI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.api != nil || r.err != nil {
		return r.api, r.err
	}
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if r.host != "" {
		opts = append(opts, client.WithHost(r.host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		r.err = err
		return nil, err
	}
	r.api = c
	return c, nil
}

// Endpoint is the Docker endpoint the resolver asks, for messages.
func (r *dockerResolver) Endpoint() string {
	api, err := r.client()
	if err != nil || api == nil {
		return r.host
	}
	return api.DaemonHost()
}

// Resolve is the container dst reaches, if any. Only a destination that can
// be a container is looked up: one on this host (loopback, or an address of
// its own), or a private or link-local address, which is where Docker puts
// container IPs. err is non-nil when Docker could not be asked.
func (r *dockerResolver) Resolve(ctx context.Context, dst netip.AddrPort) (Container, bool, error) {
	ip := dst.Addr().Unmap()
	onHost := ip.IsLoopback()
	if !onHost {
		addrs, err := r.hostAddrs()
		if err == nil {
			for _, a := range addrs {
				if a.Unmap() == ip {
					onHost = true
					break
				}
			}
		}
	}
	if !onHost && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
		return Container{}, false, nil
	}
	api, err := r.client()
	if err != nil {
		return Container{}, false, err
	}
	list, err := api.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return Container{}, false, err
	}
	for _, c := range list {
		port, ok := reaches(c, ip, dst.Port(), onHost)
		if !ok {
			continue
		}
		image, err := r.imageOf(ctx, api, c)
		if err != nil {
			return Container{}, false, err
		}
		return Container{ID: c.ID, Image: NormalizeImage(image), Port: port}, true, nil
	}
	return Container{}, false, nil
}

// reaches is the container port of c that ip:port reaches, if it reaches one:
// a TCP port c publishes at that address (any address for a port published on
// all of them), when ip is this host's, or port itself when ip is c's own on
// one of its networks.
func reaches(c container.Summary, ip netip.Addr, port uint16, onHost bool) (uint16, bool) {
	if onHost {
		for _, p := range c.Ports {
			if p.PublicPort != port || p.Type != "tcp" {
				continue
			}
			bound, err := netip.ParseAddr(p.IP)
			if p.IP == "" || (err == nil && (bound.IsUnspecified() || bound.Unmap() == ip || (bound.IsLoopback() && ip.IsLoopback()))) {
				return p.PrivatePort, true
			}
		}
	}
	if c.NetworkSettings == nil {
		return 0, false
	}
	for _, n := range c.NetworkSettings.Networks {
		if n == nil {
			continue
		}
		for _, s := range []string{n.IPAddress, n.GlobalIPv6Address} {
			if a, err := netip.ParseAddr(s); err == nil && a.Unmap() == ip {
				return port, true
			}
		}
	}
	return 0, false
}

// imageOf is the image reference c was created from, as it was given
// (Config.Image). The list's own Image field turns into an image ID once the
// tag moves to another image, so it is only the fallback.
func (r *dockerResolver) imageOf(ctx context.Context, api dockerAPI, c container.Summary) (string, error) {
	r.mu.Lock()
	image, ok := r.images[c.ID]
	r.mu.Unlock()
	if ok {
		return image, nil
	}
	got, err := api.ContainerInspect(ctx, c.ID)
	if err != nil {
		return "", err
	}
	image = c.Image
	if got.Config != nil && got.Config.Image != "" {
		image = got.Config.Image
	}
	r.mu.Lock()
	if r.images == nil {
		r.images = map[string]string{}
	}
	r.images[c.ID] = image
	r.mu.Unlock()
	return image, nil
}

// interfaceAddrs is this host's own addresses.
func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// AgentFlag is the hidden `keploy agent` flag that carries the keploy
// client's Docker endpoint to a native agent (AgentArgs, SetDockerHost).
const AgentFlag = "docker-host"

// AgentArgs is what the native launcher appends to its agent's argv: the
// endpoint DOCKER_HOST names, when the proven switch is on and it is set.
// Otherwise nothing, so the argv is what it was without container keys.
func AgentArgs() []string {
	host := os.Getenv("DOCKER_HOST")
	if !provenswitch.On() || host == "" {
		return nil
	}
	return []string{"--" + AgentFlag + "=" + host}
}
